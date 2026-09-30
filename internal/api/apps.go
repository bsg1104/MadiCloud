package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"madicloud/internal/application"
)

// AppsPath is the application collection.
const AppsPath = "/v1/apps"

const (
	// IdempotencyKeyHeader makes POST /v1/apps safe to retry.
	IdempotencyKeyHeader = "Idempotency-Key"
	// IdempotentReplayedHeader is "true" when a response replays an earlier
	// result for the same idempotency key.
	IdempotentReplayedHeader = "Idempotent-Replayed"

	// storeTimeout bounds one store operation, below the server's 10 second
	// write timeout so the client gets a JSON error rather than a reset.
	storeTimeout = 5 * time.Second
)

// Application is the wire form of an application.
type Application struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	DesiredState string    `json:"desired_state"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ApplicationList is the body of GET /v1/apps. NextCursor is present when
// more applications may follow; pass it back as ?cursor=.
type ApplicationList struct {
	Applications []Application `json:"applications"`
	NextCursor   string        `json:"next_cursor,omitempty"`
}

type createApplicationRequest struct {
	Name string `json:"name"`
}

func toWire(a application.Application) Application {
	return Application{
		ID:           a.ID,
		Name:         a.Name,
		DesiredState: string(a.DesiredState),
		CreatedAt:    a.CreatedAt.UTC(),
		UpdatedAt:    a.UpdatedAt.UTC(),
	}
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.listApps(w, r)
	case http.MethodPost:
		s.createApp(w, r)
	default:
		s.methodNotAllowed(w, r, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.getApp(w, r)
	case http.MethodDelete:
		s.deleteApp(w, r)
	default:
		s.methodNotAllowed(w, r, http.MethodGet, http.MethodHead, http.MethodDelete)
	}
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	if !s.requireJSON(w, r) {
		return
	}
	var req createApplicationRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	keys := r.Header.Values(IdempotencyKeyHeader)
	if len(keys) > 1 {
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "Idempotency-Key must be sent once")
		return
	}
	var key string
	if len(keys) == 1 {
		key = keys[0]
		if key == "" {
			s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "Idempotency-Key must not be empty")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	res, err := s.apps.Create(ctx, application.CreateInput{Name: req.Name, IdempotencyKey: key})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	app := res.Application
	msg := "application created"
	if res.Replayed {
		w.Header().Set(IdempotentReplayedHeader, "true")
		msg = "application create replayed"
	}
	s.log.Info(msg,
		"application_id", app.ID,
		"application_name", app.Name,
		"desired_state", string(app.DesiredState),
		"idempotency_key_present", key != "",
		"request_id", requestID(r.Context()),
	)
	w.Header().Set("Location", AppsPath+"/"+app.ID)
	s.writeBody(w, r, http.StatusCreated, toWire(app))
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "query string is malformed")
		return
	}
	in := application.ListInput{}
	for k, v := range q {
		if len(v) != 1 {
			s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "query parameters must not repeat")
			return
		}
		switch k {
		case "limit":
			n, err := strconv.Atoi(v[0])
			if err != nil || n < 1 || n > application.MaxListLimit {
				s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest,
					"limit must be an integer between 1 and "+strconv.Itoa(application.MaxListLimit))
				return
			}
			in.Limit = n
		case "cursor":
			after, err := decodeCursor(v[0])
			if err != nil {
				s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "cursor is invalid")
				return
			}
			in.After = after
		default:
			s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "unknown query parameter")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	page, err := s.apps.List(ctx, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := ApplicationList{Applications: make([]Application, 0, len(page.Applications))}
	for _, a := range page.Applications {
		out.Applications = append(out.Applications, toWire(a))
	}
	if page.NextAfter != "" {
		out.NextCursor = encodeCursor(page.NextAfter)
	}
	s.writeBody(w, r, http.StatusOK, out)
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	app, err := s.apps.Get(ctx, r.PathValue("id"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.writeBody(w, r, http.StatusOK, toWire(app))
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	app, err := s.apps.Delete(ctx, r.PathValue("id"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("application deleted",
		"application_id", app.ID,
		"application_name", app.Name,
		"request_id", requestID(r.Context()),
	)
	w.WriteHeader(http.StatusNoContent)
}

// writeServiceError maps application errors to responses. Anything unmapped
// is logged with the request ID and returned as a generic internal_error, so
// driver and SQL detail never reach the client.
func (s *Server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *application.ValidationError
	switch {
	case errors.As(err, &ve):
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, ve.Message)
	case errors.Is(err, application.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, codeNotFound, "application not found")
	case errors.Is(err, application.ErrNameTaken):
		s.writeError(w, r, http.StatusConflict, codeConflict, "an application with this name already exists")
	case errors.Is(err, application.ErrHasDependents):
		s.writeError(w, r, http.StatusConflict, codeConflict, "application has dependent resources")
	case errors.Is(err, application.ErrIdempotencyKeyReused):
		s.writeError(w, r, http.StatusUnprocessableEntity, codeIdempotencyKeyReused,
			"Idempotency-Key was already used with a different request")
	case errors.Is(err, application.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		s.log.Warn("application store unavailable", "request_id", requestID(r.Context()), "error", err.Error())
		s.writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "control-plane storage is unavailable")
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path,
			"request_id", requestID(r.Context()), "error", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

// Cursors are opaque to clients so the ordering key can change without an
// API version change.
func encodeCursor(after string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(after))
}

func decodeCursor(c string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", err
	}
	after := string(raw)
	if err := application.ValidateName(after); err != nil {
		return "", err
	}
	return after, nil
}
