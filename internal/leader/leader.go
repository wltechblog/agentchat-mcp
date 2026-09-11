package leader

import (
	"sync"
)

type Tracker struct {
	mu      sync.RWMutex
	leaders map[string]string
}

func NewTracker() *Tracker {
	return &Tracker{
		leaders: make(map[string]string),
	}
}

func (t *Tracker) SetInitialLeader(sessionID, creatorID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaders[sessionID] = creatorID
}

func (t *Tracker) GetLeader(sessionID string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.leaders[sessionID]
	return id, ok
}

func (t *Tracker) Transfer(sessionID, fromAgentID, toAgentID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	current, ok := t.leaders[sessionID]
	if !ok || current != fromAgentID {
		return false
	}
	t.leaders[sessionID] = toAgentID
	return true
}

func (t *Tracker) ClearSession(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.leaders, sessionID)
}

// Snapshot copies the leader map for persistence.
func (t *Tracker) Snapshot() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]string, len(t.leaders))
	for k, v := range t.leaders {
		out[k] = v
	}
	return out
}

// Restore replaces the leader map (startup only).
func (t *Tracker) Restore(leaders map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaders = make(map[string]string, len(leaders))
	for k, v := range leaders {
		t.leaders[k] = v
	}
}
