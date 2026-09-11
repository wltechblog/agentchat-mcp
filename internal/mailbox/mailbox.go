package mailbox

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

const defaultMaxPerBox = 1000

var entrySeq atomic.Int64

type Entry struct {
	ID       string            `json:"id"`
	Envelope protocol.Envelope `json:"envelope"`
	Received time.Time         `json:"received"`
}

type Box struct {
	mu      sync.Mutex
	entries []Entry
	max     int
	// changed is signaled on Deliver so long-poll waiters wake immediately.
	// Capacity 1: signals coalesce; waiters re-check on every wake.
	changed chan struct{}
}

type Store struct {
	mu     sync.RWMutex
	boxes  map[string]*Box
	maxPer int
}

func NewStore(maxPerBox int) *Store {
	if maxPerBox <= 0 {
		maxPerBox = defaultMaxPerBox
	}
	return &Store{
		boxes:  make(map[string]*Box),
		maxPer: maxPerBox,
	}
}

func (s *Store) getOrCreateBox(key string) *Box {
	s.mu.RLock()
	b, ok := s.boxes[key]
	s.mu.RUnlock()
	if ok {
		return b
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok = s.boxes[key]
	if ok {
		return b
	}
	b = &Box{max: s.maxPer, changed: make(chan struct{}, 1)}
	s.boxes[key] = b
	return b
}

// signal wakes any long-poll waiters on this box.
func (b *Box) signal() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

func (s *Store) Deliver(key string, env protocol.Envelope) {
	b := s.getOrCreateBox(key)
	e := Entry{
		ID:       fmt.Sprintf("m-%d", entrySeq.Add(1)),
		Envelope: env,
		Received: time.Now().UTC(),
	}
	b.mu.Lock()
	b.entries = append(b.entries, e)
	if len(b.entries) > b.max {
		b.entries = b.entries[len(b.entries)-b.max:]
	}
	b.mu.Unlock()
	b.signal()
}

// matchesEnvelope reports whether an envelope passes a from/type filter.
// Empty filter fields match everything.
func matchesEnvelope(env protocol.Envelope, from, msgType string) bool {
	if from != "" && env.From != from {
		return false
	}
	if msgType != "" && env.Type != msgType {
		return false
	}
	return true
}

// Drain removes and returns every queued entry (destructive read).
func (s *Store) Drain(key string) []Entry {
	s.mu.RLock()
	b, ok := s.boxes[key]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	b.mu.Lock()
	entries := b.entries
	b.entries = nil
	b.mu.Unlock()
	return entries
}

// DrainMatching removes and returns entries matching the filter. Non-matching
// entries stay queued — a filtered wait never destroys mail it didn't want.
func (s *Store) DrainMatching(key, from, msgType string) []Entry {
	s.mu.RLock()
	b, ok := s.boxes[key]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	b.mu.Lock()
	var matched, kept []Entry
	for _, e := range b.entries {
		if matchesEnvelope(e.Envelope, from, msgType) {
			matched = append(matched, e)
		} else {
			kept = append(kept, e)
		}
	}
	b.entries = kept
	b.mu.Unlock()
	return matched
}

// mailboxPollTick caps how long a long-poll waits between re-checks, so a
// coalesced wake-up signal can never leave a waiter asleep for long.
const mailboxPollTick = 500 * time.Millisecond

// DrainMatchingWait is DrainMatching with server-side long-polling: it holds
// up to wait for at least one matching entry to arrive, then returns
// immediately. With wait <= 0 it is an instantaneous DrainMatching.
func (s *Store) DrainMatchingWait(key, from, msgType string, wait time.Duration) []Entry {
	deadline := time.Now().Add(wait)
	for {
		entries := s.DrainMatching(key, from, msgType)
		if len(entries) > 0 {
			return entries
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}

		b := s.getOrCreateBox(key)
		sleep := mailboxPollTick
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-b.changed:
		case <-time.After(sleep):
		}
	}
}

func (s *Store) DeleteBox(key string) {
	s.mu.Lock()
	delete(s.boxes, key)
	s.mu.Unlock()
}

func (s *Store) Len(key string) int {
	s.mu.RLock()
	b, ok := s.boxes[key]
	s.mu.RUnlock()
	if !ok {
		return 0
	}
	b.mu.Lock()
	n := len(b.entries)
	b.mu.Unlock()
	return n
}
