// Package api is the versioned HTTP surface of the MadiCloud control plane.
//
// It exposes process liveness, control-plane readiness, and the Application
// resource. Deployment, scheduling, nodes, ingress, and MCP are not
// implemented. Unknown routes return not_found. They do not report success.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"madicloud/internal/application"
	"madicloud/internal/version"
)

const (
	// HealthPath is the process liveness endpoint. It never touches
	// dependencies.
	HealthPath = "/v1/health"
	// ReadyPath is the readiness endpoint. It checks the dependencies the
	// control plane needs to serve requests.
	ReadyPath = "/v1/ready"

	healthScopeProcess = "process"

	statusReady    = "ready"
	statusNotReady = "not_ready"

	// readyTimeout bounds one readiness check so a hung dependency cannot pin
	// request goroutines or delay shutdown for long.
	readyTimeout = 2 * time.Second

	// Stable error codes. Clients branch on these; messages are for humans
	// and may change.
	codeInvalidRequest       = "invalid_request"
	codeNotFound             = "not_found"
	codeConflict             = "conflict"
	codeIdempotencyKeyReused = "idempotency_key_reused"
	codeMethodNotAllowed     = "method_not_allowed"
	codeUnsupportedMedia     = "unsupported_media_type"
	codeRequestTooLarge      = "request_too_large"
	codeUnavailable          = "unavailable"
	codeInternal             = "internal_error"
)

// ReadinessChecker reports whether the control plane can serve requests.
// A nil error means ready. The error is logged, never returned to clients.
type ReadinessChecker interface {
	CheckReady(ctx context.Context) error
}

// ReadyResponse is the body of GET /v1/ready. It carries no dependency
// detail because the endpoint is unauthenticated.
type ReadyResponse struct {
	Status string `json:"status"`
}

var errNoReadinessChecker = errors.New("no readiness checker configured")

type readinessState int

const (
	readinessUnknown readinessState = iota
	readinessReady
	readinessNotReady
)

// HealthResponse is the body of GET /v1/health.
//
// Scope "process" means the control-plane process is serving. It does not
// mean a node, database, container runtime, or ingress path is healthy.
type HealthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
	Scope   string `json:"scope"`
}

// ErrorResponse is the body of every error response.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Server serves the control-plane HTTP API.
type Server struct {
	log             *slog.Logger
	version         string
	http            *http.Server
	shutdownTimeout time.Duration
	ready           ReadinessChecker
	apps            *application.Service

	readyMu    sync.Mutex
	readyState readinessState
}

// NewServer builds a control-plane API server. addr is recorded for ListenAndServe
// callers; Serve uses the provided listener. A nil logger discards logs. A nil
// ready checker makes /v1/ready report not_ready. A nil apps service leaves
// the /v1/apps routes unregistered.
func NewServer(addr, ver string, log *slog.Logger, ready ReadinessChecker, apps *application.Service) *Server {
	if log == nil {
		log = slog.New(slog.NewJSONHandler(discardWriter{}, nil))
	}
	if ver == "" {
		ver = version.Version
	}
	s := &Server{
		log:             log,
		version:         ver,
		shutdownTimeout: 10 * time.Second,
		ready:           ready,
		apps:            apps,
	}
	s.http = &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	return s
}

// Handler returns the API handler. It does not listen.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Serve accepts connections on ln until ctx is cancelled, then shuts down.
// Cancellation is an orderly stop and returns nil. Serve is not safe to call
// concurrently on the same Server.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.http.BaseContext = func(net.Listener) context.Context { return ctx }

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.http.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		shutErr := s.http.Shutdown(shutdownCtx)
		serveErr := <-errCh
		// Serve returns ErrServerClosed without closing ln when shutdown wins
		// the race before the listener is registered.
		_ = ln.Close()
		if shutErr != nil {
			return fmt.Errorf("shutdown http server: %w", shutErr)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", serveErr)
		}
		s.log.Info("http server stopped")
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, s.handleHealth)
	mux.HandleFunc(ReadyPath, s.handleReady)
	if s.apps != nil {
		mux.HandleFunc(AppsPath, s.handleApps)
		mux.HandleFunc(AppsPath+"/{id}", s.handleApp)
	}
	mux.HandleFunc("/", s.handleNotFound)
	return s.logRequests(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.allowGetHead(w, r) {
		return
	}
	s.writeBody(w, r, http.StatusOK, HealthResponse{
		Status:  "ok",
		Service: version.Service,
		Version: s.version,
		Scope:   healthScopeProcess,
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if !s.allowGetHead(w, r) {
		return
	}
	if err := s.checkReady(r.Context()); err != nil {
		s.writeBody(w, r, http.StatusServiceUnavailable, ReadyResponse{Status: statusNotReady})
		return
	}
	s.writeBody(w, r, http.StatusOK, ReadyResponse{Status: statusReady})
}

func (s *Server) checkReady(reqCtx context.Context) error {
	if s.ready == nil {
		s.recordReadiness(errNoReadinessChecker)
		return errNoReadinessChecker
	}
	ctx, cancel := context.WithTimeout(reqCtx, readyTimeout)
	defer cancel()
	err := s.ready.CheckReady(ctx)
	// A client that disconnected says nothing about the dependency.
	if reqCtx.Err() == nil {
		s.recordReadiness(err)
	}
	return err
}

// recordReadiness logs transitions only, so a polling load balancer does not
// produce one log line per probe.
func (s *Server) recordReadiness(err error) {
	next := readinessReady
	if err != nil {
		next = readinessNotReady
	}
	s.readyMu.Lock()
	prev := s.readyState
	s.readyState = next
	s.readyMu.Unlock()
	if prev == next {
		return
	}
	if err != nil {
		s.log.Warn("control plane not ready", "reason", err.Error())
		return
	}
	s.log.Info("control plane ready")
}

func (s *Server) allowGetHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	s.methodNotAllowed(w, r, http.MethodGet, http.MethodHead)
	return false
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	s.writeError(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	s.writeBody(w, r, status, ErrorResponse{Error: code, Message: message})
}

// writeBody writes v as JSON with an explicit Content-Length. For HEAD it
// writes the same status and headers and no body.
func (s *Server) writeBody(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Error("encode response", "path", r.URL.Path, "request_id", requestID(r.Context()), "error", err)
		status = http.StatusInternalServerError
		body = []byte(`{"error":"` + codeInternal + `","message":"internal error"}`)
	}
	// HEAD must advertise the GET representation length. Without Content-Length,
	// keep-alive clients wait for a body that never arrives.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		s.log.Error("write response", "path", r.URL.Path, "error", err)
	}
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, http.StatusNotFound, codeNotFound, "route not found")
}

// logRequests assigns a request ID, returns it in X-Request-Id, and logs one
// line per request. A client-supplied X-Request-Id is kept only if it is 1 to
// 64 characters of [A-Za-z0-9._-], so it cannot inject into logs.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(requestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
			"request_id", id,
		)
	})
}

const requestIDHeader = "X-Request-Id"

type requestIDKey struct{}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// discardWriter drops log bytes when the caller does not provide a logger.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
