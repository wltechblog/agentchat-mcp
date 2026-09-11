package presence

import (
	"sync"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

const defaultTTL = 60 * time.Second

// defaultForgetAfter is how long an expired agent's state is kept before it
// is removed entirely. Expired agents stay listed (online=false) and keep
// receiving mail until this horizon; past it they are forgotten.
const defaultForgetAfter = 24 * time.Hour

type AgentState struct {
	AgentID      string
	SessionID    string
	Capabilities []string
	LastSeen     time.Time
	// Expired is set once the TTL sweep has fired for this state and reset on
	// Touch, so the expiry callback fires exactly once per lapse.
	Expired bool
}

type ExpireFunc func(sessionID, agentID string, capabilities []string)

// ForgetFunc is called when an expired agent's state is removed entirely.
type ForgetFunc func(sessionID, agentID string)

type Tracker struct {
	mu          sync.RWMutex
	agents      map[string]*AgentState
	ttl         time.Duration
	forgetAfter time.Duration
	stopCh      chan struct{}
}

func NewTracker(ttl time.Duration) *Tracker {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Tracker{
		agents:      make(map[string]*AgentState),
		ttl:         ttl,
		forgetAfter: defaultForgetAfter,
		stopCh:      make(chan struct{}),
	}
}

func agentKey(sessionID, agentID string) string {
	return sessionID + "/" + agentID
}

func (t *Tracker) isLive(state *AgentState) bool {
	return time.Since(state.LastSeen) < t.ttl
}

// Touch records activity for an agent. It returns true only when the agent is
// seen for the first time; a returning agent that previously expired is
// revived (online again, capabilities retained) rather than re-created.
func (t *Tracker) Touch(sessionID, agentID string, capabilities []string) bool {
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
	if capabilities != nil {
		state.Capabilities = capabilities
	}
	state.Expired = false
	state.LastSeen = time.Now()
	return isNew
}

// IsPresent reports whether the agent has been seen within the TTL.
func (t *Tracker) IsPresent(sessionID, agentID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state, ok := t.agents[agentKey(sessionID, agentID)]
	if !ok {
		return false
	}
	return t.isLive(state)
}

// GetAgents returns every known agent in the session, including ones past the
// TTL. Deliverability does not depend on presence; callers use the Online flag
// to decide what to display or who is eligible for leadership.
func (t *Tracker) GetAgents(sessionID string) []protocol.AgentInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var agents []protocol.AgentInfo
	for _, state := range t.agents {
		if state.SessionID != sessionID {
			continue
		}
		agents = append(agents, protocol.AgentInfo{
			AgentID:      state.AgentID,
			Capabilities: state.Capabilities,
			Online:       t.isLive(state),
		})
	}
	return agents
}

func (t *Tracker) Remove(sessionID, agentID string) {
	t.mu.Lock()
	delete(t.agents, agentKey(sessionID, agentID))
	t.mu.Unlock()
}

// StartSweep periodically transitions agents past the TTL (firing onExpire
// once per lapse) and removes agents past the forget horizon (firing onForget).
func (t *Tracker) StartSweep(interval time.Duration, onExpire ExpireFunc, onForget ForgetFunc) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.sweep(onExpire, onForget)
			case <-t.stopCh:
				return
			}
		}
	}()
}

func (t *Tracker) Stop() {
	close(t.stopCh)
}

func (t *Tracker) sweep(onExpire ExpireFunc, onForget ForgetFunc) {
	t.mu.Lock()
	var expired, forgotten []*AgentState
	for key, state := range t.agents {
		if !state.Expired && !t.isLive(state) {
			state.Expired = true
			expired = append(expired, state)
		}
		if state.Expired && time.Since(state.LastSeen) >= t.forgetAfter {
			delete(t.agents, key)
			forgotten = append(forgotten, state)
		}
	}
	t.mu.Unlock()

	for _, state := range expired {
		onExpire(state.SessionID, state.AgentID, state.Capabilities)
	}
	for _, state := range forgotten {
		onForget(state.SessionID, state.AgentID)
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
