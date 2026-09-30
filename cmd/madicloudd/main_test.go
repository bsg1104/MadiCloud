package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"madicloud/internal/client"
	"madicloud/internal/config"
	"madicloud/internal/database"
	"madicloud/internal/database/databasetest"
	"madicloud/migrations"
)

const testPassword = "pw-91d0-not-for-logs"

func envFrom(vars map[string]string) func(string) string {
	return func(key string) string {
		if v, ok := vars[key]; ok {
			return v
		}
		if key == "MADICLOUD_DB_PASSWORD" {
			return testPassword
		}
		return ""
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestRunRejectsInvalidAddress(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), nil, envFrom(map[string]string{"MADICLOUD_HTTP_ADDR": "not-an-address"}), log)
	if err == nil || !strings.Contains(err.Error(), "MADICLOUD_HTTP_ADDR") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunRejectsMissingPassword(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), nil, envFrom(map[string]string{"MADICLOUD_DB_PASSWORD": ""}), log)
	if err == nil || !strings.Contains(err.Error(), "MADICLOUD_DB_PASSWORD is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{"deploy"}, {"serve", "extra"}} {
		if err := run(context.Background(), args, envFrom(nil), log); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("run(%v) = %v", args, err)
		}
	}
}

func TestRunWarnsWhenNotLoopback(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	err := run(ctx, nil, envFrom(map[string]string{
		"MADICLOUD_HTTP_ADDR": fmt.Sprintf("0.0.0.0:%d", port),
	}), log)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("no authentication")) {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestRunWarnsOnUnverifiedRemoteDatabase(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	err := run(ctx, nil, envFrom(map[string]string{
		"MADICLOUD_HTTP_ADDR":  fmt.Sprintf("127.0.0.1:%d", port),
		"MADICLOUD_DB_HOST":    "db.example.com",
		"MADICLOUD_DB_SSLMODE": "require",
	}), log)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("does not verify the server certificate")) {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestMigrateFailsWhenDatabaseUnreachable(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, []string{"migrate"}, envFrom(map[string]string{
		"MADICLOUD_DB_HOST": "127.0.0.1",
		"MADICLOUD_DB_PORT": strconv.Itoa(freePort(t)),
	}), log)
	if err == nil || !strings.Contains(err.Error(), "database unreachable") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), testPassword) || strings.Contains(buf.String(), testPassword) {
		t.Fatal("password leaked into error or log")
	}
}

// startServe runs serveListener in the background and returns its base URL
// and a stop function that cancels it and returns its error.
func startServe(t *testing.T, cfg config.Config, log *slog.Logger) (string, func() error) {
	t.Helper()
	set, err := database.LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTPAddr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- serveListener(ctx, cfg, set, ln, log) }()

	stop := func() error {
		cancel()
		select {
		case err := <-errCh:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("serve did not shut down")
			return nil
		}
	}
	return "http://" + ln.Addr().String(), stop
}

func statusOf(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestServeWithoutDatabase(t *testing.T) {
	t.Parallel()

	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	cfg, err := config.Load(envFrom(map[string]string{
		"MADICLOUD_DB_HOST": "127.0.0.1",
		"MADICLOUD_DB_PORT": strconv.Itoa(freePort(t)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	base, stop := startServe(t, cfg, log)

	if code, body := statusOf(t, base+"/v1/health"); code != http.StatusOK || !strings.Contains(body, `"scope":"process"`) {
		t.Fatalf("/v1/health = %d %s", code, body)
	}
	if code, body := statusOf(t, base+"/v1/ready"); code != http.StatusServiceUnavailable || body != `{"status":"not_ready"}` {
		t.Fatalf("/v1/ready = %d %s", code, body)
	}
	if code, _ := statusOf(t, base+"/v1/health"); code != http.StatusOK {
		t.Fatalf("/v1/health after failed readiness = %d", code)
	}
	if code, body := statusOf(t, base+"/v1/apps"); code != http.StatusServiceUnavailable || !strings.Contains(body, `"error":"unavailable"`) {
		t.Fatalf("/v1/apps with PostgreSQL down = %d %s", code, body)
	}
	c, err := client.New(base)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateApplication(context.Background(), "down-app", "")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable || strings.Contains(apiErr.Message, "55432") {
		t.Fatalf("create with PostgreSQL down = %v", err)
	}

	if err := stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	out := logs.String()
	if strings.Contains(out, testPassword) {
		t.Fatalf("password in logs:\n%s", out)
	}
	httpStopped := strings.Index(out, `"msg":"http server stopped"`)
	poolClosed := strings.Index(out, `"msg":"database pool closed"`)
	if httpStopped < 0 || poolClosed < 0 || poolClosed < httpStopped {
		t.Fatalf("shutdown order wrong (http=%d pool=%d):\n%s", httpStopped, poolClosed, out)
	}
	if strings.Count(out, `"msg":"database pool closed"`) != 1 {
		t.Fatalf("pool closed more than once:\n%s", out)
	}
}

func TestIntegrationMigrateThenReady(t *testing.T) {
	t.Parallel()
	dbCfg := databasetest.New(t)
	vars := map[string]string{
		"MADICLOUD_DB_HOST":     dbCfg.Host,
		"MADICLOUD_DB_PORT":     strconv.Itoa(dbCfg.Port),
		"MADICLOUD_DB_NAME":     dbCfg.Name,
		"MADICLOUD_DB_USER":     dbCfg.User,
		"MADICLOUD_DB_PASSWORD": dbCfg.Password,
		"MADICLOUD_DB_SSLMODE":  dbCfg.SSLMode,
	}
	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	cfg, err := config.Load(envFrom(vars))
	if err != nil {
		t.Fatal(err)
	}
	base, stop := startServe(t, cfg, log)

	if code, _ := statusOf(t, base+"/v1/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("/v1/ready before migrate = %d, want 503", code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range 2 {
		if err := run(ctx, []string{"migrate"}, envFrom(vars), log); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}

	if code, body := statusOf(t, base+"/v1/ready"); code != http.StatusOK || body != `{"status":"ready"}` {
		t.Fatalf("/v1/ready after migrate = %d %s", code, body)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), dbCfg.Password) {
		t.Fatal("password in logs")
	}
	if n := strings.Count(logs.String(), `"msg":"migration applied"`); n != 2 {
		t.Fatalf("migration applied %d times across two runs, want 2 (one per migration)", n)
	}
}

func dbVars(cfg config.Database) map[string]string {
	return map[string]string{
		"MADICLOUD_DB_HOST":     cfg.Host,
		"MADICLOUD_DB_PORT":     strconv.Itoa(cfg.Port),
		"MADICLOUD_DB_NAME":     cfg.Name,
		"MADICLOUD_DB_USER":     cfg.User,
		"MADICLOUD_DB_PASSWORD": cfg.Password,
		"MADICLOUD_DB_SSLMODE":  cfg.SSLMode,
	}
}

func TestIntegrationApplicationsAPI(t *testing.T) {
	t.Parallel()
	dbCfg := databasetest.New(t)
	vars := dbVars(dbCfg)
	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := context.Background()

	if err := run(ctx, []string{"migrate"}, envFrom(vars), log); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(envFrom(vars))
	if err != nil {
		t.Fatal(err)
	}

	base, stop := startServe(t, cfg, log)
	c, err := client.New(base)
	if err != nil {
		t.Fatal(err)
	}

	keep, err := c.CreateApplication(ctx, "keep-app", "")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := c.CreateApplication(ctx, "gone-app", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateApplication(ctx, "keep-app", ""); !isCode(err, 409, "conflict") {
		t.Fatalf("duplicate = %v", err)
	}
	if got, err := c.GetApplication(ctx, keep.ID); err != nil || got != keep {
		t.Fatalf("get = %+v, %v", got, err)
	}
	list, err := c.ListApplications(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "gone-app" || list[1].Name != "keep-app" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := c.DeleteApplication(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetApplication(ctx, gone.ID); !client.IsNotFound(err) {
		t.Fatalf("get deleted = %v", err)
	}
	if err := c.DeleteApplication(ctx, gone.ID); !client.IsNotFound(err) {
		t.Fatalf("second delete = %v", err)
	}

	// Explicit key: replay across a restart returns the original result.
	first, err := c.CreateApplication(ctx, "idem-app", "restart-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	base2, stop2 := startServe(t, cfg, log)
	defer func() {
		if err := stop2(); err != nil {
			t.Error(err)
		}
	}()
	c2, err := client.New(base2)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c2.GetApplication(ctx, keep.ID); err != nil || got != keep {
		t.Fatalf("after restart get = %+v, %v", got, err)
	}
	replay, err := c2.CreateApplication(ctx, "idem-app", "restart-key")
	if err != nil || replay != first {
		t.Fatalf("replay after restart = %+v, %v, want %+v", replay, err, first)
	}
	if _, err := c2.CreateApplication(ctx, "other-app", "restart-key"); !isCode(err, 422, "idempotency_key_reused") {
		t.Fatalf("reused key = %v", err)
	}
	list, err = c2.ListApplications(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("after restart list = %+v, %v", list, err)
	}

	out := logs.String()
	for _, want := range []string{`"msg":"application created"`, `"msg":"application deleted"`, `"application_id":"` + gone.ID + `"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s", want)
		}
	}
	if strings.Contains(out, dbCfg.Password) {
		t.Fatal("password in logs")
	}
}

func isCode(err error, status int, code string) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status && apiErr.Code == code
}
