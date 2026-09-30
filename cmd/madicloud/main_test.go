package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"madicloud/internal/api"
	"madicloud/internal/application"
	"madicloud/internal/application/applicationtest"
)

func apiServer(t *testing.T) string {
	t.Helper()
	svc, err := application.NewService(applicationtest.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewServer("127.0.0.1:0", "", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, svc).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func cli(t *testing.T, addr string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, func(k string) string {
		if k == "MADICLOUD_API_ADDR" {
			return addr
		}
		return ""
	}, &out, &errOut)
	return code, out.String(), errOut.String()
}

var uuidRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func TestAppLifecycle(t *testing.T) {
	t.Parallel()
	addr := apiServer(t)

	code, out, errOut := cli(t, addr, "app", "list")
	if code != 0 || out != "No applications.\n" || errOut != "" {
		t.Fatalf("empty list = %d %q %q", code, out, errOut)
	}

	code, out, errOut = cli(t, addr, "app", "create", "web-app")
	if code != 0 || !strings.HasPrefix(out, "Created application web-app\n") || errOut != "" {
		t.Fatalf("create = %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"Desired state:", "active", "Observed state:", "not tracked"} {
		if !strings.Contains(out, want) {
			t.Fatalf("create output missing %q:\n%s", want, out)
		}
	}
	id := uuidRE.FindString(out)
	if id == "" {
		t.Fatalf("no id in output:\n%s", out)
	}

	cli(t, addr, "app", "create", "api-app")
	code, out, _ = cli(t, addr, "app", "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") ||
		!strings.HasPrefix(lines[1], "api-app") || !strings.HasPrefix(lines[2], "web-app") {
		t.Fatalf("list = %d\n%s", code, out)
	}

	code, out, _ = cli(t, addr, "app", "get", id)
	if code != 0 || !strings.Contains(out, "web-app") || !strings.Contains(out, id) {
		t.Fatalf("get = %d\n%s", code, out)
	}

	code, out, _ = cli(t, addr, "app", "delete", id)
	if code != 0 || out != "Deleted application "+id+"\n" {
		t.Fatalf("delete = %d %q", code, out)
	}

	code, out, errOut = cli(t, addr, "app", "get", id)
	if code != 1 || out != "" || !strings.Contains(errOut, "application not found (not_found, HTTP 404)") || !strings.Contains(errOut, "request id: ") {
		t.Fatalf("get deleted = %d %q %q", code, out, errOut)
	}
}

func TestAppErrors(t *testing.T) {
	t.Parallel()
	addr := apiServer(t)

	cli(t, addr, "app", "create", "dup-app")
	code, _, errOut := cli(t, addr, "app", "create", "dup-app")
	if code != 1 || !strings.Contains(errOut, "already exists (conflict, HTTP 409)") {
		t.Fatalf("duplicate = %d %q", code, errOut)
	}
	code, _, errOut = cli(t, addr, "app", "create", "Bad_Name")
	if code != 1 || !strings.Contains(errOut, "invalid_request, HTTP 400") {
		t.Fatalf("invalid name = %d %q", code, errOut)
	}
	code, _, errOut = cli(t, addr, "app", "get", "not-a-uuid")
	if code != 1 || !strings.Contains(errOut, "application id must be a UUID") {
		t.Fatalf("bad id = %d %q", code, errOut)
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{},
		{"deploy"},
		{"app"},
		{"app", "scale"},
		{"app", "create"},
		{"app", "create", "a", "b"},
		{"app", "list", "extra"},
		{"app", "get"},
		{"app", "delete"},
	} {
		code, out, errOut := cli(t, "http://127.0.0.1:1", args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "Usage:") {
			t.Errorf("%v = %d %q %q", args, code, out, errOut)
		}
	}
	code, out, _ := cli(t, "", "help")
	if code != 0 || !strings.Contains(out, "MADICLOUD_API_ADDR") {
		t.Fatalf("help = %d %q", code, out)
	}
}

func TestBadAPIAddr(t *testing.T) {
	t.Parallel()
	code, _, errOut := cli(t, "http://admin:s3cret@127.0.0.1:8080", "app", "list")
	if code != 2 || !strings.Contains(errOut, "MADICLOUD_API_ADDR") || strings.Contains(errOut, "s3cret") {
		t.Fatalf("addr with credentials = %d %q", code, errOut)
	}
}

func TestAPIUnreachable(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + ln.Addr().String()
	ln.Close()

	code, _, errOut := cli(t, addr, "app", "list")
	if code != 1 || !strings.Contains(errOut, "cannot reach the MadiCloud API at "+addr) {
		t.Fatalf("unreachable = %d %q", code, errOut)
	}
}

func TestDefaultAddr(t *testing.T) {
	t.Parallel()
	// With nothing listening on the default port this fails to connect, but
	// the error names the default address.
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code := run(ctx, []string{"app", "list"}, func(string) string { return "" }, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "http://127.0.0.1:8080") {
		t.Fatalf("default addr = %d %q", code, errOut.String())
	}
}
