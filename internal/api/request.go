package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// maxRequestBody caps JSON request bodies. Application requests are a few
// dozen bytes.
const maxRequestBody = 64 << 10

// requireJSON rejects requests whose Content-Type is not application/json.
// A charset parameter is allowed only if it is utf-8.
func (s *Server) requireJSON(w http.ResponseWriter, r *http.Request) bool {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err == nil && mt == "application/json" {
		cs, ok := params["charset"]
		if !ok || strings.EqualFold(cs, "utf-8") {
			return true
		}
	}
	s.writeError(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMedia, "Content-Type must be application/json")
	return false
}

// decodeJSON reads exactly one JSON object into v, rejecting unknown fields,
// trailing data, and bodies over maxRequestBody. It writes the error response
// and returns false on failure. Messages do not echo the body.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	var tooBig *http.MaxBytesError
	err := dec.Decode(v)
	if err == nil {
		switch extra := dec.Decode(&json.RawMessage{}); {
		case errors.Is(extra, io.EOF):
		case errors.As(extra, &tooBig):
			err = extra
		default:
			err = errMultipleValues
		}
	}
	if err == nil {
		return true
	}

	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooBig):
		s.writeError(w, r, http.StatusRequestEntityTooLarge, codeRequestTooLarge, "request body must not exceed 64 KiB")
	case errors.Is(err, errMultipleValues):
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "request body must contain a single JSON object")
	case errors.Is(err, io.EOF):
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "request body is required")
	case errors.As(err, &typeErr):
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "request body has a field of the wrong type")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "request body contains an unknown field")
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "request body is not valid JSON")
	default:
		s.writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "request body is not valid JSON")
	}
	return false
}

var errMultipleValues = errors.New("multiple JSON values")
