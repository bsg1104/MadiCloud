package config_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"madicloud/internal/config"
)

const testPassword = "pw-7f3a-should-never-appear"

// env returns a getenv backed by vars. A password is supplied unless vars
// overrides MADICLOUD_DB_PASSWORD.
func env(vars map[string]string) func(string) string {
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

func TestLoadHTTPAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		set     bool
		want    string
		wantErr string
	}{
		{name: "default", want: config.DefaultHTTPAddr},
		{name: "blank uses default", value: "   ", set: true, want: config.DefaultHTTPAddr},
		{name: "loopback", value: "127.0.0.1:9090", set: true, want: "127.0.0.1:9090"},
		{name: "localhost", value: "localhost:8080", set: true, want: "localhost:8080"},
		{name: "ipv6 loopback", value: "[::1]:8080", set: true, want: "[::1]:8080"},
		{name: "all interfaces", value: ":8080", set: true, want: ":8080"},
		{name: "trimmed", value: "  127.0.0.1:8081  ", set: true, want: "127.0.0.1:8081"},
		{name: "missing port separator", value: "8080", set: true, wantErr: "invalid address"},
		{name: "empty port", value: "127.0.0.1:", set: true, wantErr: "invalid port"},
		{name: "port zero", value: ":0", set: true, wantErr: "invalid port"},
		{name: "port too high", value: ":65536", set: true, wantErr: "invalid port"},
		{name: "named port", value: "127.0.0.1:http", set: true, wantErr: "invalid port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vars := map[string]string{}
			if tt.set {
				vars["MADICLOUD_HTTP_ADDR"] = tt.value
			}
			cfg, err := config.Load(env(vars))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.HTTPAddr != tt.want {
				t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, tt.want)
			}
		})
	}
}

func TestLoadDatabaseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := config.Database{
		Host:     "localhost",
		Port:     5432,
		Name:     "madicloud",
		User:     "madicloud",
		Password: testPassword,
		SSLMode:  "disable",
	}
	if cfg.Database != want {
		t.Fatalf("Database = %v, want %v", cfg.Database, want)
	}
}

func TestLoadDatabaseOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{
		"MADICLOUD_DB_HOST":     " db.internal ",
		"MADICLOUD_DB_PORT":     "6543",
		"MADICLOUD_DB_NAME":     "control",
		"MADICLOUD_DB_USER":     "cp",
		"MADICLOUD_DB_PASSWORD": " spaced password ",
		"MADICLOUD_DB_SSLMODE":  "Require",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	db := cfg.Database
	if db.Host != "db.internal" || db.Port != 6543 || db.Name != "control" || db.User != "cp" || db.SSLMode != "require" {
		t.Fatalf("Database = %v", db)
	}
	if db.Password != " spaced password " {
		t.Fatalf("password was altered: %q", db.Password)
	}
}

func TestDefaultSSLMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host string
		want string
	}{
		{host: "localhost", want: "disable"},
		{host: "127.0.0.1", want: "disable"},
		{host: "::1", want: "disable"},
		{host: "db.example.com", want: "verify-full"},
		{host: "10.0.0.5", want: "verify-full"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(map[string]string{"MADICLOUD_DB_HOST": tt.host}))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Database.SSLMode != tt.want {
				t.Fatalf("SSLMode = %q, want %q", cfg.Database.SSLMode, tt.want)
			}
		})
	}
}

func TestLoadDatabaseInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{name: "missing password", vars: map[string]string{"MADICLOUD_DB_PASSWORD": ""}, wantErr: "MADICLOUD_DB_PASSWORD is required"},
		{name: "nul in password", vars: map[string]string{"MADICLOUD_DB_PASSWORD": "a\x00b"}, wantErr: "MADICLOUD_DB_PASSWORD contains a NUL byte"},
		{name: "port not numeric", vars: map[string]string{"MADICLOUD_DB_PORT": "abc"}, wantErr: `MADICLOUD_DB_PORT: invalid port "abc"`},
		{name: "port zero", vars: map[string]string{"MADICLOUD_DB_PORT": "0"}, wantErr: "MADICLOUD_DB_PORT: invalid port"},
		{name: "port too high", vars: map[string]string{"MADICLOUD_DB_PORT": "70000"}, wantErr: "MADICLOUD_DB_PORT: invalid port"},
		{name: "bad sslmode", vars: map[string]string{"MADICLOUD_DB_SSLMODE": "on"}, wantErr: `MADICLOUD_DB_SSLMODE: invalid value "on"`},
		{name: "socket host", vars: map[string]string{"MADICLOUD_DB_HOST": "/tmp"}, wantErr: "unix socket paths are not supported"},
		{name: "host with at", vars: map[string]string{"MADICLOUD_DB_HOST": "user@db"}, wantErr: "MADICLOUD_DB_HOST: invalid host"},
		{name: "host with space", vars: map[string]string{"MADICLOUD_DB_HOST": "db host"}, wantErr: "MADICLOUD_DB_HOST: invalid host"},
		{name: "name too long", vars: map[string]string{"MADICLOUD_DB_NAME": strings.Repeat("a", 64)}, wantErr: "MADICLOUD_DB_NAME: longer than 63 bytes"},
		{name: "user control char", vars: map[string]string{"MADICLOUD_DB_USER": "a\nb"}, wantErr: "MADICLOUD_DB_USER: contains control characters"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(env(tt.vars))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{
		"MADICLOUD_HTTP_ADDR":   "nope",
		"MADICLOUD_DB_PORT":     "x",
		"MADICLOUD_DB_PASSWORD": "",
		"MADICLOUD_DB_SSLMODE":  "bogus",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"MADICLOUD_HTTP_ADDR", "MADICLOUD_DB_PORT", "MADICLOUD_DB_PASSWORD", "MADICLOUD_DB_SSLMODE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestPasswordNotInErrors(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{
		"MADICLOUD_DB_PORT":    "bad",
		"MADICLOUD_DB_SSLMODE": "bad",
		"MADICLOUD_DB_HOST":    "bad host",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("error leaked password: %v", err)
	}

	_, err = config.Load(env(map[string]string{"MADICLOUD_DB_PASSWORD": "leak\x00me"}))
	if err == nil || strings.Contains(err.Error(), "leak") {
		t.Fatalf("error = %v", err)
	}
}

func TestPasswordNotFormattedOrLogged(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(format, cfg.Database); strings.Contains(out, testPassword) {
			t.Errorf("%s leaked password: %s", format, out)
		}
		if out := fmt.Sprintf(format, cfg); strings.Contains(out, testPassword) {
			t.Errorf("%s of Config leaked password: %s", format, out)
		}
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("config", "database", cfg.Database)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config", "database", cfg.Database)
	if strings.Contains(buf.String(), testPassword) {
		t.Fatalf("log leaked password: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"host":"localhost"`) {
		t.Fatalf("log missing host: %s", buf.String())
	}
}

func TestLoadRequiresGetenv(t *testing.T) {
	t.Parallel()
	if _, err := config.Load(nil); err == nil {
		t.Fatal("expected error for nil getenv")
	}
}

func TestLoopbackOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want bool
	}{
		{addr: "127.0.0.1:8080", want: true},
		{addr: "localhost:8080", want: true},
		{addr: "[::1]:8080", want: true},
		{addr: ":8080", want: false},
		{addr: "0.0.0.0:8080", want: false},
		{addr: "[::]:8080", want: false},
		{addr: "192.168.1.10:8080", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			cfg := config.Config{HTTPAddr: tt.addr}
			if got := cfg.LoopbackOnly(); got != tt.want {
				t.Fatalf("LoopbackOnly() = %v, want %v", got, tt.want)
			}
		})
	}
}
