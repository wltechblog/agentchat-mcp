package scratchpad

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

type Store struct {
	mu   sync.RWMutex
	pads map[string]map[string]*protocol.ScratchpadEntry
	seq  map[string]int64
}

func NewStore() *Store {
	return &Store{
		pads: make(map[string]map[string]*protocol.ScratchpadEntry),
		seq:  make(map[string]int64),
	}
}

func (s *Store) Set(sessionID, key string, value json.RawMessage, updatedBy string) protocol.ScratchpadEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pads[sessionID] == nil {
		s.pads[sessionID] = make(map[string]*protocol.ScratchpadEntry)
	}

	entry := protocol.ScratchpadEntry{
		Key:       key,
		Value:     value,
		UpdatedBy: updatedBy,
		UpdatedAt: time.Now().UTC(),
	}
	s.pads[sessionID][key] = &entry
	s.seq[sessionID]++
	return entry
}

func (s *Store) Get(sessionID, key string) (protocol.ScratchpadEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.pads[sessionID] == nil {
		return protocol.ScratchpadEntry{}, false
	}
	entry, ok := s.pads[sessionID][key]
	if !ok {
		return protocol.ScratchpadEntry{}, false
	}
	return *entry, true
}

func (s *Store) Delete(sessionID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pads[sessionID] == nil {
		return false
	}
	if _, ok := s.pads[sessionID][key]; !ok {
		return false
	}
	delete(s.pads[sessionID], key)
	s.seq[sessionID]++
	return true
}

func (s *Store) List(sessionID string) []protocol.ScratchpadEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.pads[sessionID] == nil {
		return nil
	}
	out := make([]protocol.ScratchpadEntry, 0, len(s.pads[sessionID]))
	for _, entry := range s.pads[sessionID] {
		out = append(out, *entry)
	}
	return out
}

func (s *Store) ClearSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pads, sessionID)
	delete(s.seq, sessionID)
}
