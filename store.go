package caisse

import (
	"context"
	"sync"
)

// EventStore records which Stripe events have already been handled.
//
// Stripe redelivers: it retries for days when an endpoint answers anything but
// 2xx, and it can deliver the same event twice even on the happy path. Without
// a store, "order paid" runs twice and the customer gets two parcels. This is
// the one piece caisse cannot own, because dedupe is only worth anything if it
// survives a restart — which means it belongs in the app's database.
//
// The three methods bracket one delivery:
//
//	Begin  claims the event. false means somebody already handled it.
//	Done   confirms it, after the handler returned nil.
//	Fail   releases the claim, so Stripe's retry gets another go.
//
// An implementation must make Begin atomic: two concurrent deliveries of the
// same event must not both see true. It should also let Begin reclaim a stale
// claim, or a process killed mid-handler leaves that event unprocessable
// forever. See the pg subpackage for an implementation that does both.
type EventStore interface {
	Begin(ctx context.Context, eventID string) (bool, error)
	Done(ctx context.Context, eventID string) error
	Fail(ctx context.Context, eventID string) error
}

// MemoryStore is an [EventStore] held in memory.
//
// It is for tests and local development. Using it in production means every
// deploy forgets which events were handled, and every replica dedupes against
// its own copy — so a redelivery after a restart fulfils the order a second
// time. Use the pg subpackage instead.
type MemoryStore struct {
	mutex sync.Mutex
	seen  map[string]bool
}

// NewMemoryStore returns an empty [MemoryStore].
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{seen: make(map[string]bool)}
}

// Begin claims eventID unless it is already claimed or done.
func (m *MemoryStore) Begin(_ context.Context, eventID string) (bool, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if _, exists := m.seen[eventID]; exists {
		return false, nil
	}
	m.seen[eventID] = false
	return true, nil
}

// Done marks eventID handled.
func (m *MemoryStore) Done(_ context.Context, eventID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.seen[eventID] = true
	return nil
}

// Fail releases the claim on eventID.
func (m *MemoryStore) Fail(_ context.Context, eventID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if !m.seen[eventID] {
		delete(m.seen, eventID)
	}
	return nil
}
