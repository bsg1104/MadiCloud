// Package config loads madicloudd process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"unicode"
)

const (
	envHTTPAddr   = "MADICLOUD_HTTP_ADDR"
	envDBHost     = "MADICLOUD_DB_HOST"
	envDBPort     = "MADICLOUD_DB_PORT"
	envDBName     = "MADICLOUD_DB_NAME"
	envDBUser     = "MADICLOUD_DB_USER"
	envDBPassword = "MADICLOUD_DB_PASSWORD"
	envDBSSLMode  = "MADICLOUD_DB_SSLMODE"
)

// DefaultHTTPAddr binds the unauthenticated API to loopback only.
const DefaultHTTPAddr = "127.0.0.1:8080"

// Local-development database defaults. There is no default password.
const (
	DefaultDBHost = "localhost"
	DefaultDBPort = 5432
	DefaultDBName = "madicloud"
	DefaultDBUser = "madicloud"
)

// maxIdentifierLen is PostgreSQL's NAMEDATALEN - 1.
const maxIdentifierLen = 63

var sslModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// Config is the validated process configuration.
type Config struct {
	HTTPAddr string
	Database Database
}

// Database is the control-plane PostgreSQL connection configuration.
//
// String, GoString, and LogValue omit Password so the value can be logged or
// formatted without leaking it.
type Database struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

// String describes the connection without the password.
func (d Database) String() string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=%s password=%s",
		d.Host, d.Port, d.Name, d.User, d.SSLMode, redacted(d.Password))
}

// GoString keeps %#v from printing the password.
func (d Database) GoString() string { return "config.Database{" + d.String() + "}" }

// LogValue keeps slog from printing the password.
func (d Database) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("host", d.Host),
		slog.Int("port", d.Port),
		slog.String("name", d.Name),
		slog.String("user", d.User),
		slog.String("sslmode", d.SSLMode),
	)
}

// TLSVerified reports whether the SSL mode authenticates the server.
func (d Database) TLSVerified() bool {
	return d.SSLMode == "verify-ca" || d.SSLMode == "verify-full"
}

func redacted(s string) string {
	if s == "" {
		return "(unset)"
	}
	return "(redacted)"
}

// Load reads configuration using getenv. getenv is required and is called
// only for known variable names. All validation errors are returned together.
// Errors never contain the database password.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("getenv is required")
	}

	var errs []error

	addr := strings.TrimSpace(getenv(envHTTPAddr))
	if addr == "" {
		addr = DefaultHTTPAddr
	}
	if err := validateAddr(addr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", envHTTPAddr, err))
	}

	db, dbErrs := loadDatabase(getenv)
	errs = append(errs, dbErrs...)

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return Config{HTTPAddr: addr, Database: db}, nil
}

func loadDatabase(getenv func(string) string) (Database, []error) {
	var errs []error
	db := Database{
		Host:     strings.TrimSpace(getenv(envDBHost)),
		Name:     strings.TrimSpace(getenv(envDBName)),
		User:     strings.TrimSpace(getenv(envDBUser)),
		Password: getenv(envDBPassword),
		SSLMode:  strings.ToLower(strings.TrimSpace(getenv(envDBSSLMode))),
	}

	if db.Host == "" {
		db.Host = DefaultDBHost
	}
	if err := validateDBHost(db.Host); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", envDBHost, err))
	}

	db.Port = DefaultDBPort
	if raw := strings.TrimSpace(getenv(envDBPort)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%s: invalid port %q", envDBPort, raw))
		} else {
			db.Port = n
		}
	}

	if db.Name == "" {
		db.Name = DefaultDBName
	}
	if err := validateIdentifier(db.Name); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", envDBName, err))
	}

	if db.User == "" {
		db.User = DefaultDBUser
	}
	if err := validateIdentifier(db.User); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", envDBUser, err))
	}

	switch {
	case db.Password == "":
		errs = append(errs, fmt.Errorf("%s is required", envDBPassword))
	case strings.ContainsRune(db.Password, 0):
		errs = append(errs, fmt.Errorf("%s contains a NUL byte", envDBPassword))
	}

	if db.SSLMode == "" {
		db.SSLMode = defaultSSLMode(db.Host)
	}
	if !validSSLMode(db.SSLMode) {
		errs = append(errs, fmt.Errorf("%s: invalid value %q, want one of %s",
			envDBSSLMode, db.SSLMode, strings.Join(sslModes, ", ")))
	}

	return db, errs
}

// defaultSSLMode is disable for a loopback host, where the local Compose
// PostgreSQL has no TLS, and verify-full everywhere else.
func defaultSSLMode(host string) string {
	if isLoopbackHost(host) {
		return "disable"
	}
	return "verify-full"
}

func validSSLMode(mode string) bool {
	for _, m := range sslModes {
		if mode == m {
			return true
		}
	}
	return false
}

func validateDBHost(host string) error {
	if strings.HasPrefix(host, "/") {
		return errors.New("unix socket paths are not supported")
	}
	for _, r := range host {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '@' || r == '?' || r == '#' {
			return fmt.Errorf("invalid host %q", host)
		}
	}
	return nil
}

func validateIdentifier(s string) error {
	if len(s) > maxIdentifierLen {
		return fmt.Errorf("longer than %d bytes", maxIdentifierLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("contains control characters")
		}
	}
	return nil
}

// LoopbackOnly reports whether HTTPAddr is a loopback IP or the name localhost.
// An unauthenticated listener anywhere else is reachable beyond this machine.
func (c Config) LoopbackOnly() bool {
	host, _, err := net.SplitHostPort(c.HTTPAddr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	if strings.ContainsAny(host, " \t") {
		return fmt.Errorf("invalid host %q", host)
	}
	return nil
}
