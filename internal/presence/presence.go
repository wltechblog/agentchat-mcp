package presence

import (
	"sync"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

const defaultTTL = 60 * time.Second

type AgentState struct {
	AgentID      string
	AgentName    string
	SessionID    string
	Capabilities []string
	LastSeen     time.Time
}

type ExpireFunc func(sessionID, agentID, agentName string, capabilities []string)

type Tracker struct {
	mu     sync.RWMutex
	agents map[string]*AgentState
	ttl    time.Duration
	stopCh chan struct{}
}

func NewTracker(ttl time.Duration) *Tracker {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Tracker{
		agents: make(map[string]*AgentState),
		ttl:    ttl,
		stopCh: make(chan struct{}),
	}
}

func agentKey(sessionID, agentID string) string {
	return sessionID + "/" + agentID
}

func (t *Tracker) Touch(sessionID, agentID, agentName string, capabilities []string) bool {
	key := agentKey(sessionID, agentID)
	t.mu.Lock()
	defer t.mu.Unlock()

	state, ok := t.agents[key]
	isNew := !ok
	if !ok {
		state = &AgentState{
			AgentID:   agentID,
			SessionID: sessionID,
		}
		t.agents[key] = state
	}
	if agentName != "" {
		state.AgentName = agentName
	}
	if capabilities != nil {
		state.Capabilities = capabilities
	}
	state.LastSeen = time.Now()
	return isNew
}

func (t *Tracker) IsPresent(sessionID, agentID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state, ok := t.agents[agentKey(sessionID, agentID)]
	if !ok {
		return false
	}
	return time.Since(state.LastSeen) < t.ttl
}

func (t *Tracker) GetAgents(sessionID string) []protocol.AgentInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var agents []protocol.AgentInfo
	for _, state := range t.agents {
		if state.SessionID == sessionID && time.Since(state.LastSeen) < t.ttl {
			agents = append(agents, protocol.AgentInfo{
				AgentID:      state.AgentID,
				AgentName:    state.AgentName,
				Capabilities: state.Capabilities,
			})
		}
	}
	return agents
}

func (t *Tracker) Remove(sessionID, agentID string) {
	t.mu.Lock()
	delete(t.agents, agentKey(sessionID, agentID))
	t.mu.Unlock()
}

func (t *Tracker) StartSweep(interval time.Duration, onExpire ExpireFunc) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.sweep(onExpire)
			case <-t.stopCh:
				return
			}
		}
	}()
}

func (t *Tracker) Stop() {
	close(t.stopCh)
}

func (t *Tracker) sweep(onExpire ExpireFunc) {
	t.mu.Lock()
	var expired []*AgentState
	for key, state := range t.agents {
		if time.Since(state.LastSeen) >= t.ttl {
			delete(t.agents, key)
			expired = append(expired, state)
		}
	}
	t.mu.Unlock()

	for _, state := range expired {
		onExpire(state.SessionID, state.AgentID, state.AgentName, state.Capabilities)
	}
}

func (t *Tracker) ClearSession(sessionID string) {
	t.mu.Lock()
	for key, state := range t.agents {
		if state.SessionID == sessionID {
			delete(t.agents, key)
		}
	}
	t.mu.Unlock()
}
