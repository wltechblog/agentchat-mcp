package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

const (
	maxHistory     = 100
	gracePeriod    = 30 * time.Second
	sendBufSize    = 256
	maxMessageSize = 65536
)

func agentKey(sessionID, agentID string) string {
	return sessionID + "/" + agentID
}

type Hub struct {
	mu           sync.RWMutex
	sessionStore *session.Store
	leader       *leader.Tracker
	scratchpad   *scratchpad.Store

	agents             map[string]*AgentConn
	disconnectedAgents map[string]*disconnectedInfo
	history            map[string][]protocol.Envelope
	maxHistory         int
	gracePeriod        time.Duration
	seqNums            map[string]int64
}

type disconnectedInfo struct {
	agentName    string
	capabilities []string
	timer        *time.Timer
}

type AgentConn struct {
	AgentID      string
	AgentName    string
	SessionID    string
	Capabilities []string
	conn         *websocket.Conn
	send         chan []byte
	hub          *Hub
	done         chan struct{}
}

type Option func(*Hub)

func WithGracePeriod(d time.Duration) Option {
	return func(h *Hub) { h.gracePeriod = d }
}

func WithMaxHistory(n int) Option {
	return func(h *Hub) { h.maxHistory = n }
}

func New(store *session.Store, lt *leader.Tracker, sp *scratchpad.Store, opts ...Option) *Hub {
	h := &Hub{
		sessionStore:       store,
		leader:             lt,
		scratchpad:         sp,
		agents:             make(map[string]*AgentConn),
		disconnectedAgents: make(map[string]*disconnectedInfo),
		history:            make(map[string][]protocol.Envelope),
		maxHistory:         maxHistory,
		gracePeriod:        gracePeriod,
		seqNums:            make(map[string]int64),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Hub) nextSeq(sessionID string) int64 {
	h.seqNums[sessionID]++
	return h.seqNums[sessionID]
}

func (h *Hub) Register(conn *websocket.Conn, sessionID, agentID, agentName string, capabilities []string) (*AgentConn, error) {
	key := agentKey(sessionID, agentID)

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.agents[key]; exists {
		return nil, fmt.Errorf("agent_id %q is already connected to this session", agentID)
	}

	ac := &AgentConn{
		AgentID:      agentID,
		AgentName:    agentName,
		SessionID:    sessionID,
		Capabilities: capabilities,
		conn:         conn,
		send:         make(chan []byte, sendBufSize),
		hub:          h,
		done:         make(chan struct{}),
	}

	if info, exists := h.disconnectedAgents[key]; exists {
		info.timer.Stop()
		delete(h.disconnectedAgents, key)
		h.agents[key] = ac
		slog.Info("agent reconnected", "session", sessionID, "agent", agentID)
		go h.broadcastAfterUnlock(sessionID, protocol.TypeAgentReconnected, "server", protocol.AgentInfo{
			AgentID:      agentID,
			AgentName:    agentName,
			Capabilities: capabilities,
		}, agentID)
	} else {
		h.agents[key] = ac
		slog.Info("agent joined", "session", sessionID, "agent", agentID)
		go h.broadcastAfterUnlock(sessionID, protocol.TypeAgentJoined, "server", protocol.AgentInfo{
			AgentID:      agentID,
			AgentName:    agentName,
			Capabilities: capabilities,
		}, agentID)

		if _, hasLeader := h.leader.GetLeader(sessionID); !hasLeader {
			h.leader.SetInitialLeader(sessionID, agentID)
			slog.Info("initial leader set", "session", sessionID, "leader", agentID)
		}
	}

	go ac.writePump()
	go ac.readPump()

	return ac, nil
}

func (h *Hub) Unregister(ac *AgentConn) {
	key := agentKey(ac.SessionID, ac.AgentID)

	h.mu.Lock()

	if existing, ok := h.agents[key]; !ok || existing != ac {
		h.mu.Unlock()
		return
	}
	delete(h.agents, key)

	timer := time.AfterFunc(h.gracePeriod, func() {
		h.mu.Lock()
		delete(h.disconnectedAgents, key)
		h.mu.Unlock()

		slog.Info("agent left", "session", ac.SessionID, "agent", ac.AgentID)
		h.broadcastToSession(ac.SessionID, protocol.Envelope{
			Type:      protocol.TypeAgentLeft,
			SessionID: ac.SessionID,
			From:      "server",
			Payload:   mustMarshal(protocol.AgentInfo{AgentID: ac.AgentID, AgentName: ac.AgentName, Capabilities: ac.Capabilities}),
			Timestamp: time.Now().UTC(),
		}, "")

		if leaderID, ok := h.leader.GetLeader(ac.SessionID); ok && leaderID == ac.AgentID {
			h.mu.RLock()
			var newLeader string
			for _, a := range h.agents {
				if a.SessionID == ac.SessionID {
					newLeader = a.AgentID
					break
				}
			}
			h.mu.RUnlock()

			if newLeader != "" {
				h.leader.Transfer(ac.SessionID, ac.AgentID, newLeader)
				slog.Info("leader auto-transferred", "session", ac.SessionID, "old_leader", ac.AgentID, "new_leader", newLeader)
				h.broadcastToSession(ac.SessionID, protocol.Envelope{
					Type:      protocol.TypeLeaderInfo,
					SessionID: ac.SessionID,
					From:      "server",
					Payload:   mustMarshal(map[string]string{"leader_id": newLeader}),
					Timestamp: time.Now().UTC(),
				}, "")
			}
		}
	})

	h.disconnectedAgents[key] = &disconnectedInfo{
		agentName:    ac.AgentName,
		capabilities: ac.Capabilities,
		timer:        timer,
	}
	slog.Info("agent disconnected, grace period started", "session", ac.SessionID, "agent", ac.AgentID)

	h.mu.Unlock()
}

func (h *Hub) HandleMessage(ac *AgentConn, raw []byte) {
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		ac.Send(protocol.NewError(ac.SessionID, "invalid message format"))
		return
	}
	env.From = ac.AgentID
	env.SessionID = ac.SessionID
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}

	switch env.Type {
	case protocol.TypeMessage:
		if env.To == "" {
			ac.Send(protocol.NewError(ac.SessionID, "message requires 'to' field"))
			return
		}
		env.Sequence = h.nextSeq(ac.SessionID)
		h.addToHistory(ac.SessionID, env)
		h.sendToAgent(ac.SessionID, env.To, env)

	case protocol.TypeBroadcast:
		env.Sequence = h.nextSeq(ac.SessionID)
		h.addToHistory(ac.SessionID, env)
		h.broadcastToSession(ac.SessionID, env, ac.AgentID)

	case protocol.TypeListAgents:
		agents := h.GetSessionAgents(ac.SessionID)
		resp, _ := protocol.NewEnvelope(protocol.TypeAgentsList, ac.SessionID, "server", ac.AgentID, agents)
		ac.Send(resp)

	case protocol.TypeTaskAssign, protocol.TypeTaskStatus, protocol.TypeTaskResult:
		if env.To == "" {
			ac.Send(protocol.NewError(ac.SessionID, env.Type+" requires 'to' field"))
			return
		}
		env.Sequence = h.nextSeq(ac.SessionID)
		h.addToHistory(ac.SessionID, env)
		h.sendToAgent(ac.SessionID, env.To, env)

	case protocol.TypeScratchpadSet:
		h.handleScratchpadSet(ac, env)
	case protocol.TypeScratchpadGet:
		h.handleScratchpadGet(ac, env)
	case protocol.TypeScratchpadDelete:
		h.handleScratchpadDelete(ac, env)
	case protocol.TypeScratchpadList:
		h.handleScratchpadList(ac, env)

	case protocol.TypeLeaderQuery:
		leaderID, _ := h.leader.GetLeader(ac.SessionID)
		resp, _ := protocol.NewEnvelope(protocol.TypeLeaderInfo, ac.SessionID, "server", ac.AgentID,
			map[string]string{"leader_id": leaderID})
		ac.Send(resp)

	case protocol.TypeLeaderTransfer:
		h.handleLeaderTransfer(ac, env)

	case protocol.TypeHistoryRequest:
		h.handleHistoryRequest(ac, env)

	default:
		ac.Send(protocol.NewError(ac.SessionID, "unknown message type: "+env.Type))
	}
}

func (h *Hub) handleScratchpadSet(ac *AgentConn, env protocol.Envelope) {
	var payload protocol.ScratchpadSetPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		ac.Send(protocol.NewError(ac.SessionID, "invalid scratchpad_set payload"))
		return
	}
	if payload.Key == "" {
		ac.Send(protocol.NewError(ac.SessionID, "key is required"))
		return
	}

	entry := h.scratchpad.Set(ac.SessionID, payload.Key, payload.Value, ac.AgentID)
	resp, _ := protocol.NewEnvelope(protocol.TypeScratchpadResult, ac.SessionID, "server", ac.AgentID, entry)
	ac.Send(resp)

	bcast, _ := protocol.NewEnvelope(protocol.TypeScratchpadUpdate, ac.SessionID, ac.AgentID, "", entry)
	bcast.Sequence = h.nextSeq(ac.SessionID)
	h.broadcastToSession(ac.SessionID, bcast, ac.AgentID)
}

func (h *Hub) handleScratchpadGet(ac *AgentConn, env protocol.Envelope) {
	var payload struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		ac.Send(protocol.NewError(ac.SessionID, "invalid scratchpad_get payload"))
		return
	}
	entry, ok := h.scratchpad.Get(ac.SessionID, payload.Key)
	if !ok {
		ac.Send(protocol.NewError(ac.SessionID, "key not found: "+payload.Key))
		return
	}
	resp, _ := protocol.NewEnvelope(protocol.TypeScratchpadResult, ac.SessionID, "server", ac.AgentID, entry)
	ac.Send(resp)
}

func (h *Hub) handleScratchpadDelete(ac *AgentConn, env protocol.Envelope) {
	var payload struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		ac.Send(protocol.NewError(ac.SessionID, "invalid scratchpad_delete payload"))
		return
	}
	if !h.scratchpad.Delete(ac.SessionID, payload.Key) {
		ac.Send(protocol.NewError(ac.SessionID, "key not found: "+payload.Key))
		return
	}
	resp, _ := protocol.NewEnvelope(protocol.TypeScratchpadResult, ac.SessionID, "server", ac.AgentID,
		map[string]string{"key": payload.Key, "deleted": "true"})
	ac.Send(resp)

	bcast, _ := protocol.NewEnvelope(protocol.TypeScratchpadUpdate, ac.SessionID, ac.AgentID, "",
		map[string]string{"key": payload.Key, "deleted": "true"})
	bcast.Sequence = h.nextSeq(ac.SessionID)
	h.broadcastToSession(ac.SessionID, bcast, ac.AgentID)
}

func (h *Hub) handleScratchpadList(ac *AgentConn, env protocol.Envelope) {
	entries := h.scratchpad.List(ac.SessionID)
	resp, _ := protocol.NewEnvelope(protocol.TypeScratchpadResult, ac.SessionID, "server", ac.AgentID,
		protocol.ScratchpadListResult{Entries: entries})
	ac.Send(resp)
}

func (h *Hub) handleLeaderTransfer(ac *AgentConn, env protocol.Envelope) {
	var payload protocol.LeaderTransferPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		ac.Send(protocol.NewError(ac.SessionID, "invalid leader_transfer payload"))
		return
	}

	currentLeader, _ := h.leader.GetLeader(ac.SessionID)
	if currentLeader != ac.AgentID {
		ac.Send(protocol.NewError(ac.SessionID, "only the current leader can transfer leadership"))
		return
	}

	key := agentKey(ac.SessionID, payload.NewLeaderID)
	h.mu.RLock()
	_, exists := h.agents[key]
	h.mu.RUnlock()

	if !exists {
		ac.Send(protocol.NewError(ac.SessionID, "agent not found in session: "+payload.NewLeaderID))
		return
	}

	if !h.leader.Transfer(ac.SessionID, ac.AgentID, payload.NewLeaderID) {
		ac.Send(protocol.NewError(ac.SessionID, "leadership transfer failed"))
		return
	}

	slog.Info("leader transferred", "session", ac.SessionID, "from", ac.AgentID, "to", payload.NewLeaderID)
	h.broadcastToSession(ac.SessionID, protocol.Envelope{
		Type:      protocol.TypeLeaderInfo,
		SessionID: ac.SessionID,
		From:      "server",
		Payload:   mustMarshal(map[string]string{"leader_id": payload.NewLeaderID, "transferred_by": ac.AgentID}),
		Sequence:  h.nextSeq(ac.SessionID),
		Timestamp: time.Now().UTC(),
	}, "")
}

func (h *Hub) handleHistoryRequest(ac *AgentConn, env protocol.Envelope) {
	var payload protocol.HistoryRequestPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		ac.Send(protocol.NewError(ac.SessionID, "invalid history_request payload"))
		return
	}

	if payload.Limit <= 0 {
		payload.Limit = h.maxHistory
	}

	h.mu.RLock()
	history := h.history[ac.SessionID]
	h.mu.RUnlock()

	var filtered []protocol.Envelope
	for _, e := range history {
		if e.Sequence > payload.AfterSequence {
			filtered = append(filtered, e)
		}
		if len(filtered) >= payload.Limit {
			break
		}
	}
	if filtered == nil {
		filtered = []protocol.Envelope{}
	}

	resp, _ := protocol.NewEnvelope(protocol.TypeHistoryResult, ac.SessionID, "server", ac.AgentID,
		map[string]any{"messages": filtered, "count": len(filtered)})
	ac.Send(resp)
}

func (h *Hub) GetSessionAgents(sessionID string) []protocol.AgentInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	agents := make([]protocol.AgentInfo, 0)
	for k, ac := range h.agents {
		if ac.SessionID == sessionID {
			_ = k
			agents = append(agents, protocol.AgentInfo{
				AgentID:      ac.AgentID,
				AgentName:    ac.AgentName,
				Capabilities: ac.Capabilities,
			})
		}
	}
	return agents
}

func (h *Hub) GetHistory(sessionID string) []protocol.Envelope {
	h.mu.RLock()
	defer h.mu.RUnlock()
	history := h.history[sessionID]
	out := make([]protocol.Envelope, len(history))
	copy(out, history)
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

	var channels []chan []byte
	for key, ac := range h.agents {
		if ac.SessionID == sessionID {
			channels = append(channels, ac.send)
			close(ac.done)
			ac.conn.Close()
			delete(h.agents, key)
		}
	}
	for key, info := range h.disconnectedAgents {
		info.timer.Stop()
		delete(h.disconnectedAgents, key)
	}
	delete(h.history, sessionID)
	delete(h.seqNums, sessionID)

	h.mu.Unlock()

	h.leader.ClearSession(sessionID)
	h.scratchpad.ClearSession(sessionID)

	env, _ := protocol.NewEnvelope(protocol.TypeAgentLeft, sessionID, "server", "", "session closed")
	data, _ := json.Marshal(env)
	for _, ch := range channels {
		select {
		case ch <- data:
		default:
		}
	}
}

func (h *Hub) broadcastToSession(sessionID string, env protocol.Envelope, excludeAgent string) {
	h.mu.RLock()
	var targets []chan []byte
	for _, ac := range h.agents {
		if ac.SessionID == sessionID && ac.AgentID != excludeAgent {
			targets = append(targets, ac.send)
		}
	}
	h.mu.RUnlock()

	data, _ := json.Marshal(env)
	for _, ch := range targets {
		select {
		case ch <- data:
		default:
			slog.Warn("send buffer full, dropping message")
		}
	}
}

func (h *Hub) broadcastAfterUnlock(sessionID, msgType, from string, payload any, excludeAgent string) {
	env, err := protocol.NewEnvelope(msgType, sessionID, from, "", payload)
	if err != nil {
		slog.Error("failed to create broadcast envelope", "error", err)
		return
	}
	h.broadcastToSession(sessionID, env, excludeAgent)
}

func (h *Hub) sendToAgent(sessionID, agentID string, env protocol.Envelope) {
	key := agentKey(sessionID, agentID)

	h.mu.RLock()
	ac, ok := h.agents[key]
	h.mu.RUnlock()

	if !ok {
		senderKey := agentKey(sessionID, env.From)
		h.mu.RLock()
		sender, senderOk := h.agents[senderKey]
		h.mu.RUnlock()
		if senderOk {
			sender.Send(protocol.NewError(sessionID, "agent not found: "+agentID))
		}
		return
	}

	data, _ := json.Marshal(env)
	select {
	case ac.send <- data:
	default:
		slog.Warn("send buffer full for agent", "agent", agentID)
	}
}

func (h *Hub) addToHistory(sessionID string, env protocol.Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.history[sessionID] = append(h.history[sessionID], env)
	if len(h.history[sessionID]) > h.maxHistory {
		h.history[sessionID] = h.history[sessionID][len(h.history[sessionID])-h.maxHistory:]
	}
}

func (ac *AgentConn) Send(env protocol.Envelope) {
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	select {
	case ac.send <- data:
	default:
		slog.Warn("send buffer full", "agent", ac.AgentID)
	}
}

func (ac *AgentConn) readPump() {
	defer func() {
		ac.hub.Unregister(ac)
		ac.conn.Close()
	}()

	ac.conn.SetReadLimit(maxMessageSize)
	ac.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	ac.conn.SetPongHandler(func(string) error {
		ac.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := ac.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error("read error", "agent", ac.AgentID, "error", err)
			}
			return
		}
		ac.hub.HandleMessage(ac, message)
	}
}

func (ac *AgentConn) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		ac.conn.Close()
	}()

	for {
		select {
		case message, ok := <-ac.send:
			ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				ac.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := ac.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ac.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-ac.done:
			return
		}
	}
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
