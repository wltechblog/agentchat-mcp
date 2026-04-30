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
	b = &Box{max: s.maxPer}
	s.boxes[key] = b
	return b
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
}

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
