// Package applicationtest provides an in-memory application.Store for unit
// tests of code above the store. It is not used by madicloudd. PostgreSQL
// behavior is tested against PostgreSQL in internal/database.
package applicationtest

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"madicloud/internal/application"
)

type idemRecord struct {
	hash    string
	app     application.Application
	expires time.Time
}

// MemoryStore is a mutex-guarded application.Store.
type MemoryStore struct {
	mu   sync.Mutex
	apps map[string]application.Application
	idem map[string]idemRecord
	// Err, when set, is returned by every method.
	Err error
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		apps: map[string]application.Application{},
		idem: map[string]idemRecord{},
		Now:  time.Now,
	}
}

func (m *MemoryStore) CreateApplication(_ context.Context, name string, idem *application.Idempotency) (application.Application, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return application.Application{}, false, m.Err
	}
	now := m.Now().UTC()
	if idem != nil {
		if rec, ok := m.idem[idem.Key]; ok && now.Before(rec.expires) {
			if rec.hash != idem.RequestHash {
				return application.Application{}, false, application.ErrIdempotencyKeyReused
			}
			return rec.app, true, nil
		}
	}
	for _, a := range m.apps {
		if a.Name == name {
			return application.Application{}, false, application.ErrNameTaken
		}
	}
	app := application.Application{
		ID:           newUUID(),
		Name:         name,
		DesiredState: application.DesiredActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	m.apps[app.ID] = app
	if idem != nil {
		m.idem[idem.Key] = idemRecord{hash: idem.RequestHash, app: app, expires: now.Add(idem.TTL)}
	}
	return app, false, nil
}

func (m *MemoryStore) GetApplication(_ context.Context, id string) (application.Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return application.Application{}, m.Err
	}
	a, ok := m.apps[id]
	if !ok {
		return application.Application{}, application.ErrNotFound
	}
	return a, nil
}

func (m *MemoryStore) GetApplicationByName(_ context.Context, name string) (application.Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return application.Application{}, m.Err
	}
	for _, a := range m.apps {
		if a.Name == name {
			return a, nil
		}
	}
	return application.Application{}, application.ErrNotFound
}

func (m *MemoryStore) ListApplications(_ context.Context, after string, limit int) ([]application.Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	var out []application.Application
	for _, a := range m.apps {
		if a.Name > after {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b application.Application) int { return strings.Compare(a.Name, b.Name) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) DeleteApplication(_ context.Context, id string) (application.Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return application.Application{}, m.Err
	}
	a, ok := m.apps[id]
	if !ok {
		return application.Application{}, application.ErrNotFound
	}
	delete(m.apps, id)
	return a, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
