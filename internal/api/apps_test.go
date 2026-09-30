package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"madicloud/internal/api"
	"madicloud/internal/application"
	"madicloud/internal/application/applicationtest"
)

type appsHarness struct {
	h     http.Handler
	store *applicationtest.MemoryStore
	logs  *syncBuf
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newApps(t *testing.T) *appsHarness {
	t.Helper()
	store := applicationtest.NewMemoryStore()
	svc, err := application.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	return &appsHarness{h: api.NewServer("127.0.0.1:0", "", log, nil, svc).Handler(), store: store, logs: logs}
}

type reqOpt func(*http.Request)

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func (a *appsHarness) do(t *testing.T, method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

func (a *appsHarness) create(t *testing.T, name string) api.Application {
	t.Helper()
	rec := a.do(t, http.MethodPost, "/v1/apps", `{"name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s = %d %s", name, rec.Code, rec.Body.String())
	}
	return decodeApp(t, rec.Body.Bytes())
}

func decodeApp(t *testing.T, raw []byte) api.Application {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var app api.Application
	if err := dec.Decode(&app); err != nil {
		t.Fatalf("decode application %s: %v", raw, err)
	}
	return app
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) api.ErrorResponse {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var e api.ErrorResponse
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if e.Error == "" || e.Message == "" {
		t.Fatalf("error body missing fields: %q", rec.Body.String())
	}
	return e
}

func expectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) api.ErrorResponse {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	e := decodeError(t, rec)
	if e.Error != code {
		t.Fatalf("error = %q, want %q (%s)", e.Error, code, e.Message)
	}
	return e
}

func TestCreateApp(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	rec := a.do(t, http.MethodPost, "/v1/apps", `{"name":"my-app"}`, header("X-Request-Id", "req-create-1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	app := decodeApp(t, rec.Body.Bytes())
	if app.Name != "my-app" || app.DesiredState != "active" || app.CreatedAt.IsZero() || !app.CreatedAt.Equal(app.UpdatedAt) {
		t.Fatalf("app = %+v", app)
	}
	if _, err := application.ParseID(app.ID); err != nil {
		t.Fatalf("id %q: %v", app.ID, err)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/apps/"+app.ID {
		t.Fatalf("Location = %q", loc)
	}
	if rec.Header().Get(api.IdempotentReplayedHeader) != "" {
		t.Fatal("fresh create marked replayed")
	}
	if rec.Header().Get("X-Request-Id") != "req-create-1" {
		t.Fatalf("X-Request-Id = %q", rec.Header().Get("X-Request-Id"))
	}
	if !strings.Contains(rec.Body.String(), `"desired_state":"active"`) || !strings.Contains(rec.Body.String(), `"created_at":"`) {
		t.Fatalf("wire field names changed: %s", rec.Body.String())
	}

	logs := a.logs.String()
	for _, want := range []string{`"msg":"application created"`, `"application_id":"` + app.ID + `"`, `"application_name":"my-app"`, `"request_id":"req-create-1"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %s:\n%s", want, logs)
		}
	}
}

func TestCreateAppValidation(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	tests := []struct {
		name    string
		body    string
		ctype   string
		status  int
		code    string
		message string
	}{
		{"missing name", `{}`, "", 400, "invalid_request", "name is required"},
		{"null name", `{"name":null}`, "", 400, "invalid_request", "name is required"},
		{"empty name", `{"name":""}`, "", 400, "invalid_request", "name is required"},
		{"short", `{"name":"ab"}`, "", 400, "invalid_request", "between 3 and 63"},
		{"long", `{"name":"a` + strings.Repeat("b", 63) + `"}`, "", 400, "invalid_request", "between 3 and 63"},
		{"uppercase", `{"name":"MyApp"}`, "", 400, "invalid_request", "lowercase"},
		{"number", `{"name":123}`, "", 400, "invalid_request", "wrong type"},
		{"array body", `[]`, "", 400, "invalid_request", "wrong type"},
		{"unknown field", `{"name":"my-app","image":"nginx"}`, "", 400, "invalid_request", "unknown field"},
		{"malformed", `{"name":`, "", 400, "invalid_request", "not valid JSON"},
		{"garbage", `name=my-app`, "", 400, "invalid_request", "not valid JSON"},
		{"two objects", `{"name":"one-app"}{"name":"two-app"}`, "", 400, "invalid_request", "single JSON object"},
		{"trailing garbage", `{"name":"one-app"} x`, "", 400, "invalid_request", "single JSON object"},
		{"wrong content type", `{"name":"my-app"}`, "text/plain", 415, "unsupported_media_type", "application/json"},
		{"latin1 charset", `{"name":"my-app"}`, "application/json; charset=latin1", 415, "unsupported_media_type", "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []reqOpt
			if tt.ctype != "" {
				opts = append(opts, header("Content-Type", tt.ctype))
			}
			rec := a.do(t, http.MethodPost, "/v1/apps", tt.body, opts...)
			e := expectError(t, rec, tt.status, tt.code)
			if !strings.Contains(e.Message, tt.message) {
				t.Fatalf("message = %q, want %q", e.Message, tt.message)
			}
		})
	}

	// No body at all and no Content-Type.
	req := httptest.NewRequest(http.MethodPost, "/v1/apps", nil)
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	expectError(t, rec, 415, "unsupported_media_type")

	req = httptest.NewRequest(http.MethodPost, "/v1/apps", nil)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	rec = httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	expectError(t, rec, 400, "invalid_request")

	page, _ := a.store.ListApplications(t.Context(), "", 10)
	if len(page) != 0 {
		t.Fatalf("invalid requests created %d applications", len(page))
	}
}

func TestCreateAppOversized(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	big := `{"name":"` + strings.Repeat("a", 70<<10) + `"}`
	expectError(t, a.do(t, http.MethodPost, "/v1/apps", big), http.StatusRequestEntityTooLarge, "request_too_large")

	// A valid object followed by padding past the limit is also too large.
	padded := `{"name":"ok-app"}` + strings.Repeat(" ", 70<<10) + "x"
	expectError(t, a.do(t, http.MethodPost, "/v1/apps", padded), http.StatusRequestEntityTooLarge, "request_too_large")
}

func TestCreateAppDuplicate(t *testing.T) {
	t.Parallel()
	a := newApps(t)
	a.create(t, "dup-app")
	e := expectError(t, a.do(t, http.MethodPost, "/v1/apps", `{"name":"dup-app"}`), http.StatusConflict, "conflict")
	if strings.Contains(e.Message, "23505") || strings.Contains(strings.ToLower(e.Message), "constraint") {
		t.Fatalf("message leaks database detail: %q", e.Message)
	}
}

func TestCreateAppIdempotency(t *testing.T) {
	t.Parallel()
	a := newApps(t)
	key := header(api.IdempotencyKeyHeader, "retry-123")

	first := a.do(t, http.MethodPost, "/v1/apps", `{"name":"idem-app"}`, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	// Same request with different formatting replays the same body.
	again := a.do(t, http.MethodPost, "/v1/apps", "{ \"name\" : \"idem-app\" }\n", key)
	if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() {
		t.Fatalf("replay = %d %s, want %s", again.Code, again.Body.String(), first.Body.String())
	}
	if again.Header().Get(api.IdempotentReplayedHeader) != "true" {
		t.Fatal("replay not marked")
	}

	expectError(t, a.do(t, http.MethodPost, "/v1/apps", `{"name":"other-app"}`, key), http.StatusUnprocessableEntity, "idempotency_key_reused")

	expectError(t, a.do(t, http.MethodPost, "/v1/apps", `{"name":"k-app"}`, header(api.IdempotencyKeyHeader, "has space")), 400, "invalid_request")
	expectError(t, a.do(t, http.MethodPost, "/v1/apps", `{"name":"k-app"}`, header(api.IdempotencyKeyHeader, strings.Repeat("k", 256))), 400, "invalid_request")

	req := httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"k-app"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add(api.IdempotencyKeyHeader, "a")
	req.Header.Add(api.IdempotencyKeyHeader, "b")
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	expectError(t, rec, 400, "invalid_request")

	req = httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"k-app"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header[api.IdempotencyKeyHeader] = []string{""}
	rec = httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	expectError(t, rec, 400, "invalid_request")

	list := a.do(t, http.MethodGet, "/v1/apps", "")
	var out api.ApplicationList
	if err := json.Unmarshal(list.Body.Bytes(), &out); err != nil || len(out.Applications) != 1 {
		t.Fatalf("applications after retries = %s", list.Body.String())
	}

	logs := a.logs.String()
	if n := strings.Count(logs, `"msg":"application created"`); n != 1 {
		t.Fatalf("created logged %d times, want 1", n)
	}
	if n := strings.Count(logs, `"msg":"application create replayed"`); n != 1 {
		t.Fatalf("replay logged %d times, want 1", n)
	}
}

func TestCreateAppIdempotencyConcurrentHTTP(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	const n = 10
	var wg sync.WaitGroup
	bodies := make([]string, n)
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := a.do(t, http.MethodPost, "/v1/apps", `{"name":"burst-app"}`, header(api.IdempotencyKeyHeader, "burst"))
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}()
	}
	wg.Wait()
	for i := range n {
		if codes[i] != http.StatusCreated || bodies[i] != bodies[0] {
			t.Fatalf("request %d = %d %s", i, codes[i], bodies[i])
		}
	}
}

func TestGetApp(t *testing.T) {
	t.Parallel()
	a := newApps(t)
	app := a.create(t, "get-app")

	rec := a.do(t, http.MethodGet, "/v1/apps/"+app.ID, "")
	if rec.Code != http.StatusOK || decodeApp(t, rec.Body.Bytes()) != app {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	upper := a.do(t, http.MethodGet, "/v1/apps/"+strings.ToUpper(app.ID), "")
	if upper.Code != http.StatusOK {
		t.Fatalf("upper-case id = %d", upper.Code)
	}

	head := a.do(t, http.MethodHead, "/v1/apps/"+app.ID, "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("HEAD = %d len=%s body=%q", head.Code, head.Header().Get("Content-Length"), head.Body.String())
	}

	e := expectError(t, a.do(t, http.MethodGet, "/v1/apps/00000000-0000-4000-8000-000000000000", ""), 404, "not_found")
	if e.Message != "application not found" {
		t.Fatalf("message = %q", e.Message)
	}
	headMissing := a.do(t, http.MethodHead, "/v1/apps/00000000-0000-4000-8000-000000000000", "")
	if headMissing.Code != 404 || headMissing.Body.Len() != 0 {
		t.Fatalf("HEAD missing = %d %q", headMissing.Code, headMissing.Body.String())
	}
}

func TestMalformedIDs(t *testing.T) {
	t.Parallel()
	a := newApps(t)
	a.store.Err = errors.New("store must not be reached")

	for _, id := range []string{"nope", "123", "00000000-0000-4000-8000-00000000000", "00000000000040008000000000000000", "%27%3B%20DROP%20TABLE", "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			e := expectError(t, a.do(t, method, "/v1/apps/"+id, ""), 400, "invalid_request")
			if e.Message != "application id must be a UUID" {
				t.Fatalf("%s %s message = %q", method, id, e.Message)
			}
		}
	}
	// Extra path segments are not an application route.
	expectError(t, a.do(t, http.MethodGet, "/v1/apps/00000000-0000-4000-8000-000000000000/extra", ""), 404, "not_found")
}

func TestDeleteApp(t *testing.T) {
	t.Parallel()
	a := newApps(t)
	app := a.create(t, "del-app")

	rec := a.do(t, http.MethodDelete, "/v1/apps/"+app.ID, "", header("X-Request-Id", "req-del-1"))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body.String())
	}
	expectError(t, a.do(t, http.MethodGet, "/v1/apps/"+app.ID, ""), 404, "not_found")
	expectError(t, a.do(t, http.MethodDelete, "/v1/apps/"+app.ID, ""), 404, "not_found")

	logs := a.logs.String()
	for _, want := range []string{`"msg":"application deleted"`, `"application_id":"` + app.ID + `"`, `"application_name":"del-app"`, `"request_id":"req-del-1"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %s", want)
		}
	}
}

func TestListApps(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	empty := a.do(t, http.MethodGet, "/v1/apps", "")
	if empty.Code != 200 || empty.Body.String() != `{"applications":[]}` {
		t.Fatalf("empty list = %d %s", empty.Code, empty.Body.String())
	}

	for _, n := range []string{"charlie", "alpha", "bravo", "a-z", "delta"} {
		a.create(t, n)
	}

	var names []string
	cursor := ""
	pages := 0
	for {
		path := "/v1/apps?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := a.do(t, http.MethodGet, path, "")
		if rec.Code != 200 {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		var out api.ApplicationList
		dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out); err != nil {
			t.Fatal(err)
		}
		pages++
		for _, app := range out.Applications {
			names = append(names, app.Name)
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if strings.Join(names, ",") != "a-z,alpha,bravo,charlie,delta" || pages != 3 {
		t.Fatalf("names = %v in %d pages", names, pages)
	}

	first := a.do(t, http.MethodGet, "/v1/apps", "").Body.String()
	for range 5 {
		if got := a.do(t, http.MethodGet, "/v1/apps", "").Body.String(); got != first {
			t.Fatal("list order is not deterministic")
		}
	}

	head := a.do(t, http.MethodHead, "/v1/apps", "")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(first)) {
		t.Fatalf("HEAD list = %d %s", head.Code, head.Header().Get("Content-Length"))
	}
}

func TestListAppsBadQuery(t *testing.T) {
	t.Parallel()
	a := newApps(t)
	for _, q := range []string{"limit=0", "limit=501", "limit=abc", "limit=-1", "limit=1&limit=2", "cursor=%%%", "cursor=QUJD", "cursor=!!", "sort=name", "offset=10"} {
		expectError(t, a.do(t, http.MethodGet, "/v1/apps?"+q, ""), 400, "invalid_request")
	}
	if rec := a.do(t, http.MethodGet, "/v1/apps?limit=500", ""); rec.Code != 200 {
		t.Fatalf("limit=500 = %d", rec.Code)
	}
}

func TestAppsMethodNotAllowed(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	rec := a.do(t, http.MethodDelete, "/v1/apps", "")
	expectError(t, rec, 405, "method_not_allowed")
	if rec.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
	rec = a.do(t, http.MethodPut, "/v1/apps/00000000-0000-4000-8000-000000000000", `{"name":"x"}`)
	expectError(t, rec, 405, "method_not_allowed")
	if rec.Header().Get("Allow") != "GET, HEAD, DELETE" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestAppsStoreErrorsAreNotLeaked(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	a.store.Err = errors.New(`postgres 42P01: relation "applications" does not exist; password=hunter2`)
	for _, rec := range []*httptest.ResponseRecorder{
		a.do(t, http.MethodGet, "/v1/apps", ""),
		a.do(t, http.MethodPost, "/v1/apps", `{"name":"leak-app"}`),
		a.do(t, http.MethodGet, "/v1/apps/00000000-0000-4000-8000-000000000000", ""),
		a.do(t, http.MethodDelete, "/v1/apps/00000000-0000-4000-8000-000000000000", ""),
	} {
		e := expectError(t, rec, 500, "internal_error")
		if e.Message != "internal error" || strings.Contains(rec.Body.String(), "postgres") || strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	}
	if !strings.Contains(a.logs.String(), `"msg":"request failed"`) {
		t.Fatal("internal error was not logged")
	}

	a.store.Err = application.ErrUnavailable
	expectError(t, a.do(t, http.MethodGet, "/v1/apps", ""), 503, "unavailable")
	expectError(t, a.do(t, http.MethodPost, "/v1/apps", `{"name":"down-app"}`), 503, "unavailable")

	a.store.Err = application.ErrHasDependents
	expectError(t, a.do(t, http.MethodDelete, "/v1/apps/00000000-0000-4000-8000-000000000000", ""), 409, "conflict")
}

func TestAppsRoutesAbsentWithoutService(t *testing.T) {
	t.Parallel()
	h := api.NewServer("127.0.0.1:0", "", discardLog(), nil, nil).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	expectError(t, rec, 404, "not_found")
}

func TestRequestIDGeneratedAndSanitized(t *testing.T) {
	t.Parallel()
	a := newApps(t)

	rec := a.do(t, http.MethodGet, "/v1/health", "")
	id := rec.Header().Get("X-Request-Id")
	if id == "" || len(id) > 64 {
		t.Fatalf("generated id = %q", id)
	}
	rec = a.do(t, http.MethodGet, "/v1/health", "", header("X-Request-Id", "bad id\nforged=1"))
	if got := rec.Header().Get("X-Request-Id"); got == "" || strings.ContainsAny(got, " \n=") {
		t.Fatalf("unsafe request id kept: %q", got)
	}
	if strings.Contains(a.logs.String(), "forged") {
		t.Fatal("client request id injected into logs")
	}
}

func TestHealthUnaffectedByStore(t *testing.T) {
	t.Parallel()
	a := newApps(t)
	a.store.Err = application.ErrUnavailable
	if rec := a.do(t, http.MethodGet, "/v1/health", ""); rec.Code != 200 {
		t.Fatalf("/v1/health with store down = %d", rec.Code)
	}
}

func FuzzCreateAppBody(f *testing.F) {
	for _, seed := range []string{`{"name":"my-app"}`, `{`, `[]`, `null`, `{"name":1}`, `{"name":"a"}{"b":2}`, "\xff\xfe", `{"name":"\u0000"}`} {
		f.Add(seed)
	}
	store := applicationtest.NewMemoryStore()
	svc, err := application.NewService(store)
	if err != nil {
		f.Fatal(err)
	}
	h := api.NewServer("127.0.0.1:0", "", discardLog(), nil, svc).Handler()
	f.Fuzz(func(t *testing.T, body string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case 201, 400, 409, 413:
		default:
			t.Fatalf("status %d for %q", rec.Code, body)
		}
		if !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("response is not JSON: %q", rec.Body.String())
		}
	})
}
