package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"madicloud/internal/api"
	"madicloud/internal/application"
	"madicloud/internal/application/applicationtest"
	"madicloud/internal/client"
)

func apiHandler(t *testing.T) (http.Handler, *applicationtest.MemoryStore) {
	t.Helper()
	store := applicationtest.NewMemoryStore()
	svc, err := application.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return api.NewServer("127.0.0.1:0", "", log, nil, svc).Handler(), store
}

func newClient(t *testing.T, url string) *client.Client {
	t.Helper()
	c, err := client.New(url, client.WithRetryBackoff(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewValidatesAddress(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"http://127.0.0.1:8080", "127.0.0.1:8080", "https://api.example.com/base/", "localhost:9"} {
		if _, err := client.New(ok); err != nil {
			t.Errorf("New(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ftp://x", "http://user:secret@host", "http://host?x=1", "http://host#f", "http://", "://x"} {
		c, err := client.New(bad)
		if err == nil {
			t.Errorf("New(%q) accepted: %s", bad, c.BaseURL())
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks credentials: %v", err)
		}
	}
}

func TestApplicationLifecycle(t *testing.T) {
	t.Parallel()
	h, _ := apiHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(t, srv.URL)
	ctx := context.Background()

	app, err := c.CreateApplication(ctx, "cli-app", "")
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "cli-app" || app.DesiredState != "active" || app.ID == "" {
		t.Fatalf("created = %+v", app)
	}
	got, err := c.GetApplication(ctx, app.ID)
	if err != nil || got != app {
		t.Fatalf("get = %+v, %v", got, err)
	}
	list, err := c.ListApplications(ctx)
	if err != nil || len(list) != 1 || list[0] != app {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := c.DeleteApplication(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetApplication(ctx, app.ID)
	if !client.IsNotFound(err) {
		t.Fatalf("get after delete = %v", err)
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.Message != "application not found" || apiErr.RequestID == "" {
		t.Fatalf("APIError = %+v", apiErr)
	}
}

func TestListFollowsPagination(t *testing.T) {
	t.Parallel()
	h, store := apiHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	const n = application.DefaultListLimit + 25
	for i := range n {
		if _, _, err := store.CreateApplication(context.Background(), fmt.Sprintf("app-%04d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	list, err := newClient(t, srv.URL).ListApplications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n || list[0].Name != "app-0000" || list[n-1].Name != fmt.Sprintf("app-%04d", n-1) {
		t.Fatalf("got %d applications", len(list))
	}
}

func TestCreateErrorsAreStructured(t *testing.T) {
	t.Parallel()
	h, _ := apiHandler(t)
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)

	_, err := c.CreateApplication(context.Background(), "Bad Name", "")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || apiErr.Code != "invalid_request" || apiErr.Message == "" {
		t.Fatalf("err = %v", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("4xx was retried: %d attempts", posts.Load())
	}

	if _, err := c.CreateApplication(context.Background(), "dup-app", ""); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateApplication(context.Background(), "dup-app", "")
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.Code != "conflict" {
		t.Fatalf("duplicate = %v", err)
	}
}

// The first create succeeds on the server but its response is lost. The
// retry must replay the same application rather than create a second one or
// report a name conflict.
func TestCreateRetriesLostResponseWithSameKey(t *testing.T) {
	t.Parallel()
	h, store := apiHandler(t)

	var mu sync.Mutex
	var keys []string
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			if dropped.CompareAndSwap(false, true) {
				h.ServeHTTP(httptest.NewRecorder(), r)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	app, err := newClient(t, srv.URL).CreateApplication(context.Background(), "lost-app", "")
	if err != nil {
		t.Fatalf("create after lost response: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("idempotency keys = %q", keys)
	}
	all, _ := store.ListApplications(context.Background(), "", 10)
	if len(all) != 1 || all[0].ID != app.ID {
		t.Fatalf("stored = %+v, returned %+v", all, app)
	}
}

func TestCreateRetriesServerErrorsThenGivesUp(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"unavailable","message":"control-plane storage is unavailable"}`)
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).CreateApplication(context.Background(), "down-app", "")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "unavailable" {
		t.Fatalf("err = %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestCreateStopsWhenContextCancelled(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		// The server notices a client disconnect only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := newClient(t, srv.URL).CreateApplication(ctx, "slow-app", ""); err == nil {
		t.Fatal("expected error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts after caller deadline = %d, want 1", attempts.Load())
	}
}

func TestPathEscaping(t *testing.T) {
	t.Parallel()
	h, _ := apiHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, err := newClient(t, srv.URL).GetApplication(context.Background(), "a/b")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_request" {
		t.Fatalf("id with slash = %v, want invalid_request (one path segment)", err)
	}
}

func TestDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var elsewhere atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Store(true) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).ListApplications(context.Background())
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v", err)
	}
	if elsewhere.Load() {
		t.Fatal("client followed a redirect")
	}
}

func TestNonJSONErrorBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway from proxy", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).GetApplication(context.Background(), "00000000-0000-4000-8000-000000000000")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 502 || apiErr.Code != "" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestSendsHeaders(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"x","name":"hdr-app","desired_state":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()

	c, err := client.New(srv.URL, client.WithUserAgent("madicloud-cli/test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateApplication(context.Background(), "hdr-app", "my-key"); err != nil {
		t.Fatal(err)
	}
	h := <-seen
	if h.Get("Content-Type") != "application/json" || h.Get("Accept") != "application/json" ||
		h.Get("Idempotency-Key") != "my-key" || h.Get("User-Agent") != "madicloud-cli/test" || h.Get("Authorization") != "" {
		t.Fatalf("headers = %v", h)
	}
}
