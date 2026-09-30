// Package application defines the Application resource: its desired state,
// validation rules, and the service that creates, reads, lists, and deletes
// it.
//
// This package knows nothing about HTTP, PostgreSQL, containers, or
// scheduling. Storage is reached through Store.
package application

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Name length limits. The maximum is one DNS label, so a name can later be
// used as a hostname label without translation.
const (
	NameMinLen = 3
	NameMaxLen = 63
)

// DesiredState is what the operator wants to be true of an application.
//
// Transitions:
//
//	(none)  --create-->  active
//	active  --delete-->  deleted --(dependents gone)--> row removed
//
// With no dependent resources in this phase, delete goes from active to
// removed in one step and deleted is never persisted. Later phases record
// deleted while controllers tear down dependents.
//
// There is no observed state yet: nothing runs an application, so nothing
// can observe one.
type DesiredState string

const (
	DesiredActive  DesiredState = "active"
	DesiredDeleted DesiredState = "deleted"
)

// Valid reports whether s is a known desired state.
func (s DesiredState) Valid() bool {
	return s == DesiredActive || s == DesiredDeleted
}

// Application is the desired-state record of one application.
type Application struct {
	ID           string
	Name         string
	DesiredState DesiredState
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

var (
	// ErrNotFound means no application matches.
	ErrNotFound = errors.New("application not found")
	// ErrNameTaken means another application already has the name.
	ErrNameTaken = errors.New("application name is already in use")
	// ErrHasDependents means other resources still reference the application.
	ErrHasDependents = errors.New("application has dependent resources")
	// ErrIdempotencyKeyReused means the key was already used for a different
	// request.
	ErrIdempotencyKeyReused = errors.New("idempotency key was already used with a different request")
	// ErrUnavailable means the store could not be reached. The operation may
	// be retried.
	ErrUnavailable = errors.New("application store is unavailable")
)

// ValidationError is invalid caller input. Message is safe to show to the
// caller and does not echo the input.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// ValidateName enforces the application name policy:
//
//   - 3 to 63 characters
//   - lowercase ASCII letters, digits, and hyphens only
//   - starts with a letter, ends with a letter or digit
//   - no consecutive hyphens
//
// Uppercase is rejected, not folded, so a name has exactly one spelling.
// Names are unique across the cluster. PostgreSQL enforces the same rules.
func ValidateName(name string) error {
	if name == "" {
		return invalid("name", "name is required")
	}
	if len(name) < NameMinLen || len(name) > NameMaxLen {
		return invalid("name", "name must be between %d and %d characters", NameMinLen, NameMaxLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return invalid("name", "name may contain only lowercase letters, digits, and hyphens")
		}
	}
	if name[0] < 'a' || name[0] > 'z' {
		return invalid("name", "name must start with a lowercase letter")
	}
	if name[len(name)-1] == '-' {
		return invalid("name", "name must end with a lowercase letter or digit")
	}
	if strings.Contains(name, "--") {
		return invalid("name", "name must not contain consecutive hyphens")
	}
	return nil
}

// ParseID validates a UUID in canonical 8-4-4-4-12 form and returns it in
// lowercase. Other encodings (braces, URN prefix, no hyphens) are rejected.
func ParseID(s string) (string, error) {
	if len(s) != 36 {
		return "", invalid("id", "application id must be a UUID")
	}
	b := []byte(s)
	for i, c := range b {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", invalid("id", "application id must be a UUID")
			}
			continue
		}
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
			b[i] = c + ('a' - 'A')
		default:
			return "", invalid("id", "application id must be a UUID")
		}
	}
	return string(b), nil
}
