package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// List limits.
const (
	DefaultListLimit = 100
	MaxListLimit     = 500
)

// Idempotency key limits and retention.
const (
	IdempotencyKeyMaxLen = 255
	// IdempotencyTTL is how long a key replays its original result. After it
	// expires the key may be reused for any request.
	IdempotencyTTL = 24 * time.Hour
)

// Store persists applications. Implementations must enforce name uniqueness
// atomically and return this package's errors for the conditions they name.
type Store interface {
	// CreateApplication inserts an active application. With a non-nil idem,
	// the insert and the key record commit together, a live key with the same
	// RequestHash returns the recorded application with replayed true, and a
	// live key with a different RequestHash returns ErrIdempotencyKeyReused.
	CreateApplication(ctx context.Context, name string, idem *Idempotency) (app Application, replayed bool, err error)
	GetApplication(ctx context.Context, id string) (Application, error)
	GetApplicationByName(ctx context.Context, name string) (Application, error)
	// ListApplications returns up to limit applications with name greater
	// than after, ordered by name in byte order.
	ListApplications(ctx context.Context, after string, limit int) ([]Application, error)
	// DeleteApplication removes the application and returns it. It returns
	// ErrHasDependents if other resources reference it.
	DeleteApplication(ctx context.Context, id string) (Application, error)
}

// Idempotency identifies a retried create.
type Idempotency struct {
	Key         string
	RequestHash string
	TTL         time.Duration
}

// CreateInput is a create request. IdempotencyKey is optional.
type CreateInput struct {
	Name           string
	IdempotencyKey string
}

// CreateResult is a created application. Replayed is true when the result was
// recorded by an earlier request with the same idempotency key.
type CreateResult struct {
	Application Application
	Replayed    bool
}

// ListInput selects one page. Limit 0 means DefaultListLimit. After is the
// last name of the previous page, or empty for the first page.
type ListInput struct {
	Limit int
	After string
}

// Page is one page of applications ordered by name. NextAfter is set when more
// applications may follow.
type Page struct {
	Applications []Application
	NextAfter    string
}

// Service implements application operations over a Store.
type Service struct {
	store Store
}

// NewService returns a Service backed by store.
func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("application store is required")
	}
	return &Service{store: store}, nil
}

// Create validates in and creates an active application.
//
// Without an idempotency key, a retried request after a lost response can
// fail with ErrNameTaken even though the first attempt succeeded. Callers
// that retry must send a key.
func (s *Service) Create(ctx context.Context, in CreateInput) (CreateResult, error) {
	if err := ValidateName(in.Name); err != nil {
		return CreateResult{}, err
	}
	var idem *Idempotency
	if in.IdempotencyKey != "" {
		if err := ValidateIdempotencyKey(in.IdempotencyKey); err != nil {
			return CreateResult{}, err
		}
		idem = &Idempotency{
			Key:         in.IdempotencyKey,
			RequestHash: createRequestHash(in.Name),
			TTL:         IdempotencyTTL,
		}
	}
	app, replayed, err := s.store.CreateApplication(ctx, in.Name, idem)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Application: app, Replayed: replayed}, nil
}

// Get returns the application with id.
func (s *Service) Get(ctx context.Context, id string) (Application, error) {
	id, err := ParseID(id)
	if err != nil {
		return Application{}, err
	}
	return s.store.GetApplication(ctx, id)
}

// GetByName returns the application named name.
func (s *Service) GetByName(ctx context.Context, name string) (Application, error) {
	if err := ValidateName(name); err != nil {
		return Application{}, err
	}
	return s.store.GetApplicationByName(ctx, name)
}

// List returns one page of applications ordered by name.
func (s *Service) List(ctx context.Context, in ListInput) (Page, error) {
	limit := in.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return Page{}, invalid("limit", "limit must be between 1 and %d", MaxListLimit)
	}
	if in.After != "" {
		if err := ValidateName(in.After); err != nil {
			return Page{}, invalid("after", "list position is invalid")
		}
	}

	apps, err := s.store.ListApplications(ctx, in.After, limit+1)
	if err != nil {
		return Page{}, err
	}
	page := Page{Applications: apps}
	if len(apps) > limit {
		page.Applications = apps[:limit]
		page.NextAfter = apps[limit-1].Name
	}
	return page, nil
}

// Delete removes the application with id and returns what was removed.
//
// This phase has no resources that depend on an application, so deletion is
// immediate. The store still refuses with ErrHasDependents if a reference
// exists. When deployments exist, Delete will instead record DesiredDeleted
// and leave removal to a controller; callers already treat success as
// "deletion accepted".
func (s *Service) Delete(ctx context.Context, id string) (Application, error) {
	id, err := ParseID(id)
	if err != nil {
		return Application{}, err
	}
	return s.store.DeleteApplication(ctx, id)
}

// ValidateIdempotencyKey accepts 1 to 255 visible ASCII characters.
func ValidateIdempotencyKey(key string) error {
	if key == "" || len(key) > IdempotencyKeyMaxLen {
		return invalid("idempotency_key", "Idempotency-Key must be 1 to %d characters", IdempotencyKeyMaxLen)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return invalid("idempotency_key", "Idempotency-Key may contain only visible ASCII characters")
		}
	}
	return nil
}

// createRequestHash identifies the semantic content of a create request, so a
// retry with different JSON formatting still matches.
func createRequestHash(name string) string {
	sum := sha256.Sum256([]byte("applications.create\x00" + name))
	return hex.EncodeToString(sum[:])
}
