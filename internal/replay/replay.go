// Package replay remembers token IDs until the tokens expire.
package replay

import (
	"sync"
	"time"
)

// pruneInterval is how often Seen drops the entries that have expired.
const pruneInterval = time.Minute

// Set stores the IDs seen so far. Call New to make one.
type Set struct {
	now func() time.Time

	mu        sync.Mutex
	seen      map[string]time.Time
	nextPrune time.Time
}

// New returns an empty set that reads the time from now.
func New(now func() time.Time) *Set {
	return &Set{now: now, seen: make(map[string]time.Time), nextPrune: now().Add(pruneInterval)}
}

// Seen records id until expiry and reports whether it was already recorded
// and had not expired.
func (s *Set) Seen(id string, expiry time.Time) bool {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if !now.Before(s.nextPrune) {
		for key, until := range s.seen {
			if !now.Before(until) {
				delete(s.seen, key)
			}
		}
		s.nextPrune = now.Add(pruneInterval)
	}
	if until, ok := s.seen[id]; ok && now.Before(until) {
		return true
	}
	s.seen[id] = expiry
	return false
}

// Len is the number of entries stored.
// Expired entries count until the next prune.
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}
