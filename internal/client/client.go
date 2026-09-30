// Package client is a Go client for the MadiCloud HTTP API.
//
// It talks only to the API. It has no database or runtime access.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds one HTTP attempt.
	DefaultTimeout = 15 * time.Second

	// maxResponseBody caps how much of a response is read.
	maxResponseBody = 4 << 20

	createAttempts = 3
)

// Application is an application as returned by the API.
type Application struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	DesiredState string    `json:"desired_state"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// APIError is a structured error response from the API.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Code == "" {
		return fmt.Sprintf("%s (HTTP %d)", msg, e.StatusCode)
	}
	return fmt.Sprintf("%s (%s, HTTP %d)", msg, e.Code, e.StatusCode)
}

// IsNotFound reports whether err is an API not_found error.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == "not_found"
}

// Client calls one MadiCloud API endpoint. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string
	backoff   time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client. Its redirect policy is replaced so
// requests are never forwarded to another location.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		copied := *hc
		c.http = &copied
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// WithRetryBackoff sets the delay before the first create retry. It doubles
// for each further retry.
func WithRetryBackoff(d time.Duration) Option { return func(c *Client) { c.backoff = d } }

// New returns a client for the API at baseURL, for example
// "http://127.0.0.1:8080". A value without a scheme is treated as http.
// URLs carrying credentials, a query, or a fragment are rejected.
func New(baseURL string, opts ...Option) (*Client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, errors.New("API address is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("API address is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("API address scheme must be http or https, not %q", u.Scheme)
	case u.User != nil:
		return nil, errors.New("API address must not contain credentials")
	case u.Host == "":
		return nil, errors.New("API address has no host")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("API address must not contain a query or fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")

	c := &Client{
		base:      u,
		http:      &http.Client{Timeout: DefaultTimeout},
		userAgent: "madicloud-client",
		backoff:   250 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c, nil
}

// BaseURL returns the API address.
func (c *Client) BaseURL() string { return c.base.String() }

// CreateApplication creates an application.
//
// Every call sends an Idempotency-Key. If idempotencyKey is empty a random
// one is generated. Attempts that fail with a network error or a 5xx response
// are retried with the same key, so a lost response cannot create a second
// application.
func (c *Client) CreateApplication(ctx context.Context, name, idempotencyKey string) (Application, error) {
	if idempotencyKey == "" {
		idempotencyKey = "cli-" + rand.Text()
	}
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return Application{}, err
	}

	var lastErr error
	delay := c.backoff
	for attempt := 1; attempt <= createAttempts; attempt++ {
		var app Application
		err := c.do(ctx, http.MethodPost, appsPath, nil, body,
			map[string]string{"Idempotency-Key": idempotencyKey}, http.StatusCreated, &app)
		if err == nil {
			return app, nil
		}
		lastErr = err
		if ctx.Err() != nil || !retryable(err) || attempt == createAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return Application{}, lastErr
		case <-time.After(delay):
		}
		delay *= 2
	}
	return Application{}, lastErr
}

// GetApplication returns the application with id.
func (c *Client) GetApplication(ctx context.Context, id string) (Application, error) {
	var app Application
	err := c.do(ctx, http.MethodGet, appPath(id), nil, nil, nil, http.StatusOK, &app)
	return app, err
}

// apiPath is a request path in decoded and escaped form, so an ID containing
// "/" or "%" stays one path segment.
type apiPath struct{ decoded, escaped string }

var appsPath = apiPath{"/v1/apps", "/v1/apps"}

func appPath(id string) apiPath {
	return apiPath{"/v1/apps/" + id, "/v1/apps/" + url.PathEscape(id)}
}

// ListApplications returns every application, following pagination.
func (c *Client) ListApplications(ctx context.Context) ([]Application, error) {
	var all []Application
	cursor := ""
	for {
		q := url.Values{}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Applications []Application `json:"applications"`
			NextCursor   string        `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, appsPath, q, nil, nil, http.StatusOK, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Applications...)
		if page.NextCursor == "" {
			return all, nil
		}
		if page.NextCursor == cursor {
			return nil, errors.New("API returned the same cursor twice")
		}
		cursor = page.NextCursor
	}
}

// DeleteApplication deletes the application with id.
func (c *Client) DeleteApplication(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, appPath(id), nil, nil, nil, http.StatusNoContent, nil)
}

func (c *Client) do(ctx context.Context, method string, p apiPath, query url.Values, body []byte, headers map[string]string, want int, out any) error {
	u := *c.base
	u.Path = c.base.Path + p.decoded
	u.RawPath = c.base.EscapedPath() + p.escaped
	u.RawQuery = query.Encode()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return &transportError{err: fmt.Errorf("read response: %w", err)}
	}

	if resp.StatusCode != want {
		apiErr := &APIError{StatusCode: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil {
			apiErr.Code, apiErr.Message = e.Error, e.Message
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, p.decoded, err)
	}
	return nil
}

// transportError is a failure before a complete response was received,
// including a per-attempt timeout.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// retryable reports whether a create attempt may be repeated with the same
// idempotency key. 4xx responses are final.
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	var te *transportError
	return errors.As(err, &te)
}
