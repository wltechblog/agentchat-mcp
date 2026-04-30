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
	mu           sync.RWMutex
	sessionStore *session.Store
	leader       *leader.Tracker
	scratchpad   *scratchpad.Store
	files        *filestore.Store
	presence     *presence.Tracker
	mailboxes    *mailbox.Store
	history      map[string][]protocol.Envelope
	maxHistory   int
	seqNums      map[string]int64
	debugLog     bool
}

type Option func(*Hub)

func WithMaxHistory(n int) Option {
	return func(h *Hub) { h.maxHistory = n }
}

func WithDebugLog(debug bool) Option {
	return func(h *Hub) { h.debugLog = debug }
}

func New(store *session.Store, lt *leader.Tracker, sp *scratchpad.Store, fs *filestore.Store, pt *presence.Tracker, mb *mailbox.Store, opts ...Option) *Hub {
	h := &Hub{
		sessionStore: store,
		leader:       lt,
		scratchpad:   sp,
		files:        fs,
		presence:     pt,
		mailboxes:    mb,
		history:      make(map[string][]protocol.Envelope),
		maxHistory:   maxHistory,
		seqNums:      make(map[string]int64),
	}
	for _, opt := range opts {
		opt(h)
	}

	pt.StartSweep(15*time.Second, func(sessionID, agentID, agentName string, capabilities []string) {
		h.onAgentExpired(sessionID, agentID, agentName, capabilities)
	})

	return h
}

func (h *Hub) nextSeq(sessionID string) int64 {
	h.seqNums[sessionID]++
	return h.seqNums[sessionID]
}

func (h *Hub) Register(sessionID, agentID, agentName string, capabilities []string) bool {
	isNew := h.presence.Touch(sessionID, agentID, agentName, capabilities)

	if isNew {
		slog.Info("agent joined", "session", sessionID, "agent", agentID)
		h.deliverToSessionMailboxes(sessionID, protocol.Envelope{
			Type:      protocol.TypeAgentJoined,
			SessionID: sessionID,
			From:      "server",
			Payload:   mustMarshal(protocol.AgentInfo{AgentID: agentID, AgentName: agentName, Capabilities: capabilities}),
			Timestamp: time.Now().UTC(),
		}, agentID)

		if _, hasLeader := h.leader.GetLeader(sessionID); !hasLeader {
			h.leader.SetInitialLeader(sessionID, agentID)
			slog.Info("initial leader set", "session", sessionID, "leader", agentID)
		}
	}

	return isNew
}

func (h *Hub) onAgentExpired(sessionID, agentID, agentName string, capabilities []string) {
	slog.Info("agent expired", "session", sessionID, "agent", agentID)
	h.deliverToSessionMailboxes(sessionID, protocol.Envelope{
		Type:      protocol.TypeAgentLeft,
		SessionID: sessionID,
		From:      "server",
		Payload:   mustMarshal(protocol.AgentInfo{AgentID: agentID, AgentName: agentName, Capabilities: capabilities}),
		Timestamp: time.Now().UTC(),
	}, "")

	if leaderID, ok := h.leader.GetLeader(sessionID); ok && leaderID == agentID {
		agents := h.presence.GetAgents(sessionID)
		if len(agents) > 0 {
			newLeader := agents[0].AgentID
			h.leader.Transfer(sessionID, agentID, newLeader)
			slog.Info("leader auto-transferred", "session", sessionID, "old_leader", agentID, "new_leader", newLeader)
			h.deliverToSessionMailboxes(sessionID, protocol.Envelope{
				Type:      protocol.TypeLeaderInfo,
				SessionID: sessionID,
				From:      "server",
				Payload:   mustMarshal(map[string]string{"leader_id": newLeader}),
				Timestamp: time.Now().UTC(),
			}, "")
		}
	}

	h.mailboxes.DeleteBox(sessionID + "/" + agentID)
}

func (h *Hub) DrainMailbox(sessionID, agentID string) []mailbox.Entry {
	h.presence.Touch(sessionID, agentID, "", nil)
	return h.mailboxes.Drain(sessionID + "/" + agentID)
}

func (h *Hub) SendMessage(sessionID, from, to, msgType string, payload json.RawMessage) error {
	if to == "" {
		return fmt.Errorf("'to' is required")
	}
	env := protocol.Envelope{
		Type:      msgType,
		SessionID: sessionID,
		From:      from,
		To:        to,
		Payload:   payload,
		Sequence:  h.nextSeq(sessionID),
		Timestamp: time.Now().UTC(),
	}
	h.addToHistory(sessionID, env)
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
		Sequence:  h.nextSeq(sessionID),
		Timestamp: time.Now().UTC(),
	}
	h.addToHistory(sessionID, env)
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
		Sequence:  h.nextSeq(sessionID),
		Timestamp: time.Now().UTC(),
	}
	h.addToHistory(sessionID, env)
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
	h.mu.RLock()
	history := h.history[sessionID]
	h.mu.RUnlock()

	if limit <= 0 {
		limit = h.maxHistory
	}

	var filtered []protocol.Envelope
	for _, e := range history {
		if e.Sequence > afterSeq {
			filtered = append(filtered, e)
		}
		if len(filtered) >= limit {
			break
		}
	}
	if filtered == nil {
		filtered = []protocol.Envelope{}
	}
	return filtered
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

func (h *Hub) addToHistory(sessionID string, env protocol.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.history[sessionID] = append(h.history[sessionID], env)
	if len(h.history[sessionID]) > h.maxHistory {
		h.history[sessionID] = h.history[sessionID][len(h.history[sessionID])-h.maxHistory:]
	}
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
