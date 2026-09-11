package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/filestore"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/presence"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

const maxHistory = 100

type Hub struct {
	mu            sync.RWMutex
	sessionStore  *session.Store
	leader        *leader.Tracker
	scratchpad    *scratchpad.Store
	files         *filestore.Store
	presence      *presence.Tracker
	mailboxes     *mailbox.Store
	history       map[string][]protocol.Envelope
	maxHistory    int
	sweepInterval time.Duration
	seqNums       map[string]int64
	debugLog      bool
}

type Option func(*Hub)

func WithMaxHistory(n int) Option {
	return func(h *Hub) { h.maxHistory = n }
}

func WithDebugLog(debug bool) Option {
	return func(h *Hub) { h.debugLog = debug }
}

// WithSweepInterval sets how often expired agents are detected. The default
// is 15s; tests use a much shorter interval.
func WithSweepInterval(d time.Duration) Option {
	return func(h *Hub) { h.sweepInterval = d }
}

func New(store *session.Store, lt *leader.Tracker, sp *scratchpad.Store, fs *filestore.Store, pt *presence.Tracker, mb *mailbox.Store, opts ...Option) *Hub {
	h := &Hub{
		sessionStore:  store,
		leader:        lt,
		scratchpad:    sp,
		files:         fs,
		presence:      pt,
		mailboxes:     mb,
		history:       make(map[string][]protocol.Envelope),
		maxHistory:    maxHistory,
		sweepInterval: 15 * time.Second,
		seqNums:       make(map[string]int64),
	}
	for _, opt := range opts {
		opt(h)
	}

	pt.StartSweep(h.sweepInterval, func(sessionID, agentID string, capabilities []string) {
		h.onAgentExpired(sessionID, agentID, capabilities)
	}, func(sessionID, agentID string) {
		// The agent is being forgotten entirely (past the presence tracker's
		// forget horizon); only now is its mailbox reclaimed.
		h.mailboxes.DeleteBox(sessionID + "/" + agentID)
	})

	return h
}

// nextSeq returns the next sequence number for a session.
func (h *Hub) nextSeq(sessionID string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seqNums[sessionID]++
	return h.seqNums[sessionID]
}

// recordEnvelope assigns the next sequence number and appends the envelope to
// the session history atomically, so history order always matches sequence
// order. seqNums and history share h.mu; the counter must never be bumped
// without the lock (concurrent map writes are a fatal runtime error).
func (h *Hub) recordEnvelope(sessionID string, env *protocol.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.seqNums[sessionID]++
	env.Sequence = h.seqNums[sessionID]
	h.history[sessionID] = append(h.history[sessionID], *env)
	if len(h.history[sessionID]) > h.maxHistory {
		h.history[sessionID] = h.history[sessionID][len(h.history[sessionID])-h.maxHistory:]
	}
}

func (h *Hub) Register(sessionID, agentID string, capabilities []string) bool {
	isNew := h.presence.Touch(sessionID, agentID, capabilities)

	if isNew {
		slog.Info("agent joined", "session", sessionID, "agent", agentID)
		h.deliverToSessionMailboxes(sessionID, protocol.Envelope{
			Type:      protocol.TypeAgentJoined,
			SessionID: sessionID,
			From:      "server",
			Payload:   mustMarshal(protocol.AgentInfo{AgentID: agentID, Capabilities: capabilities, Online: true}),
			Timestamp: time.Now().UTC(),
		}, agentID)

		if _, hasLeader := h.leader.GetLeader(sessionID); !hasLeader {
			h.leader.SetInitialLeader(sessionID, agentID)
			slog.Info("initial leader set", "session", sessionID, "leader", agentID)
		}
	}

	return isNew
}

func (h *Hub) RefreshPresence(sessionID, agentID string) {
	h.presence.Touch(sessionID, agentID, nil)
}

func (h *Hub) onAgentExpired(sessionID, agentID string, capabilities []string) {
	slog.Info("agent expired", "session", sessionID, "agent", agentID)
	h.deliverToSessionMailboxes(sessionID, protocol.Envelope{
		Type:      protocol.TypeAgentLeft,
		SessionID: sessionID,
		From:      "server",
		Payload:   mustMarshal(protocol.AgentInfo{AgentID: agentID, Capabilities: capabilities, Online: false}),
		Timestamp: time.Now().UTC(),
	}, "")

	// The agent's mailbox is intentionally kept: messages sent while it is
	// offline must still be there when it returns. The mailbox is only
	// reclaimed when the agent is forgotten entirely (see the onForget hook).

	if leaderID, ok := h.leader.GetLeader(sessionID); ok && leaderID == agentID {
		newLeader := ""
		for _, a := range h.presence.GetAgents(sessionID) {
			if a.Online {
				newLeader = a.AgentID
				break
			}
		}
		if newLeader != "" {
			h.leader.Transfer(sessionID, agentID, newLeader)
			slog.Info("leader auto-transferred", "session", sessionID, "old_leader", agentID, "new_leader", newLeader)
			h.deliverToSessionMailboxes(sessionID, protocol.Envelope{
				Type:      protocol.TypeLeaderInfo,
				SessionID: sessionID,
				From:      "server",
				Payload:   mustMarshal(map[string]string{"leader_id": newLeader}),
				Timestamp: time.Now().UTC(),
			}, "")
		} else {
			// Nobody online to lead; clear so the next agent to join leads.
			h.leader.ClearSession(sessionID)
		}
	}
}

func (h *Hub) DrainMailbox(sessionID, agentID string) []mailbox.Entry {
	h.presence.Touch(sessionID, agentID, nil)
	return h.mailboxes.Drain(sessionID + "/" + agentID)
}

func (h *Hub) SendMessage(sessionID, from, to, msgType string, payload json.RawMessage) error {
	if to == "" {
		return fmt.Errorf("'to' is required")
	}
	// Delivery does not depend on presence: an offline target's mailbox
	// accepts the message and it will be there when the target returns.
	env := protocol.Envelope{
		Type:      msgType,
		SessionID: sessionID,
		From:      from,
		To:        to,
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	}
	h.recordEnvelope(sessionID, &env)
	h.deliverToAgentMailbox(sessionID, to, env)
	return nil
}

func (h *Hub) Broadcast(sessionID, from, msgType string, payload json.RawMessage) {
	env := protocol.Envelope{
		Type:      msgType,
		SessionID: sessionID,
		From:      from,
		To:        "",
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	}
	h.recordEnvelope(sessionID, &env)
	h.deliverToSessionMailboxes(sessionID, env, from)
}

func (h *Hub) deliverToAgentMailbox(sessionID, agentID string, env protocol.Envelope) {
	key := sessionID + "/" + agentID
	h.mailboxes.Deliver(key, env)
	if h.debugLog {
		slog.Info("delivered to mailbox", "agent", agentID, "type", env.Type)
	}
}

func (h *Hub) deliverToSessionMailboxes(sessionID string, env protocol.Envelope, excludeAgent string) {
	agents := h.presence.GetAgents(sessionID)
	for _, a := range agents {
		if a.AgentID != excludeAgent {
			h.deliverToAgentMailbox(sessionID, a.AgentID, env)
		}
	}
}

func (h *Hub) ScratchpadSet(sessionID, agentID, key string, value json.RawMessage) (protocol.ScratchpadEntry, error) {
	if key == "" {
		return protocol.ScratchpadEntry{}, fmt.Errorf("key is required")
	}
	entry := h.scratchpad.Set(sessionID, key, value, agentID)

	bcast, _ := protocol.NewEnvelope(protocol.TypeScratchpadUpdate, sessionID, agentID, "", entry)
	bcast.Sequence = h.nextSeq(sessionID)
	h.deliverToSessionMailboxes(sessionID, bcast, agentID)

	return entry, nil
}

func (h *Hub) ScratchpadGet(sessionID, key string) (protocol.ScratchpadEntry, error) {
	entry, ok := h.scratchpad.Get(sessionID, key)
	if !ok {
		return protocol.ScratchpadEntry{}, fmt.Errorf("key not found: %s", key)
	}
	return entry, nil
}

func (h *Hub) ScratchpadDelete(sessionID, agentID, key string) error {
	if !h.scratchpad.Delete(sessionID, key) {
		return fmt.Errorf("key not found: %s", key)
	}
	bcast, _ := protocol.NewEnvelope(protocol.TypeScratchpadUpdate, sessionID, agentID, "",
		map[string]string{"key": key, "deleted": "true"})
	bcast.Sequence = h.nextSeq(sessionID)
	h.deliverToSessionMailboxes(sessionID, bcast, agentID)
	return nil
}

func (h *Hub) ScratchpadList(sessionID string) []protocol.ScratchpadEntry {
	return h.scratchpad.List(sessionID)
}

func (h *Hub) LeaderTransfer(sessionID, fromAgent, newLeaderID string) error {
	currentLeader, _ := h.leader.GetLeader(sessionID)
	if currentLeader != fromAgent {
		return fmt.Errorf("only the current leader can transfer leadership")
	}

	if !h.presence.IsPresent(sessionID, newLeaderID) {
		return fmt.Errorf("agent not found in session: %s", newLeaderID)
	}

	if !h.leader.Transfer(sessionID, fromAgent, newLeaderID) {
		return fmt.Errorf("leadership transfer failed")
	}

	slog.Info("leader transferred", "session", sessionID, "from", fromAgent, "to", newLeaderID)

	h.deliverToSessionMailboxes(sessionID, protocol.Envelope{
		Type:      protocol.TypeLeaderInfo,
		SessionID: sessionID,
		From:      "server",
		Payload:   mustMarshal(map[string]string{"leader_id": newLeaderID, "transferred_by": fromAgent}),
		Sequence:  h.nextSeq(sessionID),
		Timestamp: time.Now().UTC(),
	}, fromAgent)

	return nil
}

func (h *Hub) ShareFile(sessionID, from, to, fileID, fileName, contentType, description string, size int64) error {
	if to == "" {
		return fmt.Errorf("'to' is required")
	}
	if _, ok := h.files.Get(sessionID, fileID); !ok {
		return fmt.Errorf("file not found: %s", fileID)
	}

	payload, _ := json.Marshal(protocol.FileSharePayload{
		FileID: fileID, FileName: fileName, ContentType: contentType, Size: size, Description: description,
	})
	env := protocol.Envelope{
		Type:      protocol.TypeFileShare,
		SessionID: sessionID,
		From:      from,
		To:        to,
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	}
	h.recordEnvelope(sessionID, &env)
	h.deliverToAgentMailbox(sessionID, to, env)
	slog.Info("file shared", "session", sessionID, "from", from, "to", to, "file", fileName, "file_id", fileID)
	return nil
}

func (h *Hub) GetFiles(sessionID string) []*filestore.File {
	return h.files.List(sessionID)
}

func (h *Hub) StoreFile(sessionID, filename, contentType, uploadedBy string, data []byte) (*filestore.File, error) {
	return h.files.Store(sessionID, filename, contentType, uploadedBy, data)
}

func (h *Hub) GetFile(sessionID, fileID string) (*filestore.File, bool) {
	return h.files.Get(sessionID, fileID)
}

func (h *Hub) DeleteFile(sessionID, fileID string) bool {
	return h.files.Delete(sessionID, fileID)
}

func (h *Hub) GetSessionAgents(sessionID string) []protocol.AgentInfo {
	return h.presence.GetAgents(sessionID)
}

func (h *Hub) GetHistory(sessionID string) []protocol.Envelope {
	h.mu.RLock()
	defer h.mu.RUnlock()
	history := h.history[sessionID]
	out := make([]protocol.Envelope, len(history))
	copy(out, history)
	return out
}

func (h *Hub) GetHistoryAfter(sessionID string, afterSeq int64, limit int) []protocol.Envelope {
	if limit <= 0 {
		limit = h.maxHistory
	}

	// Copy the matching slice under the lock: iterating the live slice after
	// releasing it races with concurrent appends into the same backing array.
	h.mu.RLock()
	out := make([]protocol.Envelope, 0, limit)
	for _, e := range h.history[sessionID] {
		if e.Sequence > afterSeq {
			out = append(out, e)
		}
		if len(out) >= limit {
			break
		}
	}
	h.mu.RUnlock()
	return out
}

func (h *Hub) GetScratchpad(sessionID string) []protocol.ScratchpadEntry {
	return h.scratchpad.List(sessionID)
}

func (h *Hub) GetLeader(sessionID string) string {
	id, _ := h.leader.GetLeader(sessionID)
	return id
}

func (h *Hub) CloseSession(sessionID string) {
	h.mu.Lock()
	delete(h.history, sessionID)
	delete(h.seqNums, sessionID)
	h.mu.Unlock()

	h.presence.ClearSession(sessionID)
	h.leader.ClearSession(sessionID)
	h.scratchpad.ClearSession(sessionID)
	h.files.ClearSession(sessionID)
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
