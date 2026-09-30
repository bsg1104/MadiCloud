package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"madicloud/internal/api"
)

// leakyError stands in for a driver error that names internal endpoints.
const leakyError = "failed to connect to user=madicloud host=db.internal.example password=hunter2"

type fakeChecker struct {
	calls       atomic.Int32
	mu          sync.Mutex
	err         error
	sawDeadline atomic.Bool
}

func (f *fakeChecker) CheckReady(ctx context.Context) error {
	f.calls.Add(1)
	if _, ok := ctx.Deadline(); ok {
		f.sawDeadline.Store(true)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeChecker) set(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestReadyWhenCheckerPasses(t *testing.T) {
	t.Parallel()

	checker := &fakeChecker{}
	h := api.NewServer("127.0.0.1:0", "", discardLog(), checker, nil).Handler()

	rec := get(t, h, http.MethodGet, api.ReadyPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ready"}` {
		t.Fatalf("body = %s", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", rec.Header())
	}
	if !checker.sawDeadline.Load() {
		t.Fatal("readiness check ran without a deadline")
	}
}

func TestReadyWhenCheckerFails(t *testing.T) {
	t.Parallel()

	checker := &fakeChecker{err: errors.New(leakyError)}
	h := api.NewServer("127.0.0.1:0", "", discardLog(), checker, nil).Handler()

	rec := get(t, h, http.MethodGet, api.ReadyPath)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if body != `{"status":"not_ready"}` {
		t.Fatalf("body = %s", body)
	}
	for _, secret := range []string{"hunter2", "db.internal", "user=", "failed to connect"} {
		if strings.Contains(body, secret) || strings.Contains(strings.Join(headerValues(rec.Header()), " "), secret) {
			t.Fatalf("response exposed %q", secret)
		}
	}
}

func TestReadyWithoutChecker(t *testing.T) {
	t.Parallel()

	h := api.NewServer("127.0.0.1:0", "", discardLog(), nil, nil).Handler()
	rec := get(t, h, http.MethodGet, api.ReadyPath)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestReadyHead(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "ready", status: http.StatusOK},
		{name: "not ready", err: errors.New("down"), status: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := api.NewServer("127.0.0.1:0", "", discardLog(), &fakeChecker{err: tt.err}, nil).Handler()
			head := get(t, h, http.MethodHead, api.ReadyPath)
			getRec := get(t, h, http.MethodGet, api.ReadyPath)
			if head.Code != tt.status || getRec.Code != tt.status {
				t.Fatalf("HEAD = %d GET = %d, want %d", head.Code, getRec.Code, tt.status)
			}
			if head.Body.Len() != 0 {
				t.Fatalf("HEAD body = %q", head.Body.String())
			}
			if head.Header().Get("Content-Length") != strconv.Itoa(getRec.Body.Len()) {
				t.Fatalf("HEAD Content-Length = %q, GET body = %d", head.Header().Get("Content-Length"), getRec.Body.Len())
			}
		})
	}
}

func TestReadyMethodNotAllowed(t *testing.T) {
	t.Parallel()

	checker := &fakeChecker{}
	h := api.NewServer("127.0.0.1:0", "", discardLog(), checker, nil).Handler()
	rec := get(t, h, http.MethodPost, api.ReadyPath)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("status = %d Allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
	assertErrorCode(t, rec.Body.Bytes(), "method_not_allowed")
	if checker.calls.Load() != 0 {
		t.Fatal("POST ran the readiness check")
	}
}

func TestHealthIndependentOfReadiness(t *testing.T) {
	t.Parallel()

	checker := &fakeChecker{err: errors.New("database down")}
	h := api.NewServer("127.0.0.1:0", "", discardLog(), checker, nil).Handler()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := get(t, h, method, api.HealthPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /v1/health = %d with readiness failing", method, rec.Code)
		}
	}
	if n := checker.calls.Load(); n != 0 {
		t.Fatalf("/v1/health called the readiness checker %d times", n)
	}
}

func TestReadinessLogsTransitionsOnly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil))

	checker := &fakeChecker{err: errors.New(leakyError)}
	h := api.NewServer("127.0.0.1:0", "", log, checker, nil).Handler()

	for range 3 {
		get(t, h, http.MethodGet, api.ReadyPath)
	}
	checker.set(nil)
	for range 3 {
		get(t, h, http.MethodGet, api.ReadyPath)
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if n := strings.Count(out, `"msg":"control plane not ready"`); n != 1 {
		t.Fatalf("not ready logged %d times:\n%s", n, out)
	}
	if n := strings.Count(out, `"msg":"control plane ready"`); n != 1 {
		t.Fatalf("ready logged %d times:\n%s", n, out)
	}
	if !strings.Contains(out, `"reason":"failed to connect`) {
		t.Fatalf("reason not logged:\n%s", out)
	}
}

func TestReadyCancelledClientDoesNotFlipState(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	checker := &blockingChecker{}
	h := api.NewServer("127.0.0.1:0", "", log, checker, nil).Handler()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, api.ReadyPath, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readiness handler ignored client cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "control plane not ready") {
		t.Fatalf("client disconnect was logged as not ready:\n%s", buf.String())
	}
}

type blockingChecker struct{}

func (blockingChecker) CheckReady(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func headerValues(h http.Header) []string {
	var out []string
	for _, v := range h {
		out = append(out, v...)
	}
	return out
}
