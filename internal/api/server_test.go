package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"madicloud/internal/api"
	"madicloud/internal/version"
)

func TestHealth(t *testing.T) {
	t.Parallel()

	srv := api.NewServer("127.0.0.1:0", "", discardLog(), nil, nil)
	req := httptest.NewRequest(http.MethodGet, api.HealthPath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}

	var body api.HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.Service != version.Service || body.Version != version.Version || body.Scope != "process" {
		t.Fatalf("body = %+v", body)
	}
}

func TestHealthHeadHasNoBody(t *testing.T) {
	t.Parallel()

	srv := api.NewServer("127.0.0.1:0", "9.9.9", discardLog(), nil, nil)
	req := httptest.NewRequest(http.MethodHead, api.HealthPath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q", rec.Body.String())
	}

	get := httptest.NewRecorder()
	srv.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, api.HealthPath, nil))
	if rec.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
		t.Fatalf("HEAD Content-Length = %q, GET body length = %d", rec.Header().Get("Content-Length"), get.Body.Len())
	}
}

func TestHealthMethodNotAllowed(t *testing.T) {
	t.Parallel()

	srv := api.NewServer("127.0.0.1:0", "0.1.0", discardLog(), nil, nil)
	req := httptest.NewRequest(http.MethodPost, api.HealthPath, strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("Allow = %q", allow)
	}
	assertErrorCode(t, rec.Body.Bytes(), "method_not_allowed")
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	t.Parallel()

	srv := api.NewServer("127.0.0.1:0", "0.1.0", discardLog(), nil, nil)
	for _, path := range []string{"/", "/v1/apps", "/v1/health/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
		assertErrorCode(t, rec.Body.Bytes(), "not_found")
	}
}

func TestRequestLog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	srv := api.NewServer("127.0.0.1:0", "0.1.0", log, nil, nil)
	req := httptest.NewRequest(http.MethodGet, api.HealthPath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !bytes.Contains(buf.Bytes(), []byte(`"path":"/v1/health"`)) || !bytes.Contains(buf.Bytes(), []byte(`"status":200`)) {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestServeShutdown(t *testing.T) {
	srv := api.NewServer("127.0.0.1:0", "0.1.0", discardLog(), nil, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx, ln)
	}()

	url := "http://" + ln.Addr().String() + api.HealthPath
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timed out")
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	if _, err := client.Get(url); err == nil {
		t.Fatal("expected listener to be closed after shutdown")
	}
}

func TestServeCancelledContext(t *testing.T) {
	srv := api.NewServer("127.0.0.1:0", "0.1.0", discardLog(), nil, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx, ln)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timed out")
	}
}

func assertErrorCode(t *testing.T, raw []byte, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error != code {
		t.Fatalf("error = %q, want %q", body.Error, code)
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
