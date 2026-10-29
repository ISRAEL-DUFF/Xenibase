package backup

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Ephemeral holds secrets an operation needs only while it runs (an import
// source URL), in this process's memory: never in the metadata DB, gone on
// restart (spec §6.8 notes).
type Ephemeral struct {
	mu sync.Mutex
	m  map[uuid.UUID]ephemeralEntry
}

type ephemeralEntry struct {
	value   string
	expires time.Time
}

// NewEphemeral returns an empty store.
func NewEphemeral() *Ephemeral { return &Ephemeral{m: map[uuid.UUID]ephemeralEntry{}} }

// Put stores value for id until ttl passes.
func (e *Ephemeral) Put(id uuid.UUID, value string, ttl time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for k, v := range e.m {
		if now.After(v.expires) {
			delete(e.m, k)
		}
	}
	e.m[id] = ephemeralEntry{value: value, expires: now.Add(ttl)}
}

// Get returns the value for id, if it is still held.
func (e *Ephemeral) Get(id uuid.UUID) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.m[id]
	if !ok || time.Now().After(v.expires) {
		return "", false
	}
	return v.value, true
}

// Delete forgets id.
func (e *Ephemeral) Delete(id uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, id)
}
