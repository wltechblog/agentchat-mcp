package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wltechblog/agentchat-mcp/internal/hub"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type Handler struct {
	hub          *hub.Hub
	sessionStore *session.Store
	leader       *leader.Tracker
	scratchpad   *scratchpad.Store
}

func New(h *hub.Hub, store *session.Store, lt *leader.Tracker, sp *scratchpad.Store) *Handler {
	return &Handler{
		hub:          h,
		sessionStore: store,
		leader:       lt,
		scratchpad:   sp,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sessions", h.createSession)
	mux.HandleFunc("GET /sessions", h.listSessions)
	mux.HandleFunc("GET /sessions/{id}", h.getSession)
	mux.HandleFunc("DELETE /sessions/{id}", h.deleteSession)
	mux.HandleFunc("GET /sessions/{id}/agents", h.listAgents)
	mux.HandleFunc("GET /sessions/{id}/history", h.getHistory)
	mux.HandleFunc("GET /sessions/{id}/leader", h.getLeader)
	mux.HandleFunc("GET /sessions/{id}/scratchpad", h.getScratchpad)
	mux.HandleFunc("GET /ws", h.handleWebSocket)
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	sess := h.sessionStore.Create(req.Name)
	slog.Info("session created", "id", sess.ID, "name", sess.Name)

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         sess.ID,
		"name":       sess.Name,
		"psk":        sess.PSK,
		"created_at": sess.CreatedAt,
	})
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions := h.sessionStore.List()
	out := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, map[string]any{
			"id":          s.ID,
			"name":        s.Name,
			"created_at":  s.CreatedAt,
			"agent_count": len(h.hub.GetSessionAgents(s.ID)),
			"leader_id":   h.hub.GetLeader(s.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := h.sessionStore.Get(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          sess.ID,
		"name":        sess.Name,
		"created_at":  sess.CreatedAt,
		"agent_count": len(h.hub.GetSessionAgents(sess.ID)),
		"leader_id":   h.hub.GetLeader(sess.ID),
	})
}

func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.sessionStore.Delete(id) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	h.hub.CloseSession(id)
	slog.Info("session deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.sessionStore.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	agents := h.hub.GetSessionAgents(id)
	writeJSON(w, http.StatusOK, agents)
}

func (h *Handler) getHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.sessionStore.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	history := h.hub.GetHistory(id)
	writeJSON(w, http.StatusOK, history)
}

func (h *Handler) getLeader(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.sessionStore.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	leaderID := h.hub.GetLeader(id)
	writeJSON(w, http.StatusOK, map[string]string{"leader_id": leaderID})
}

func (h *Handler) getScratchpad(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.sessionStore.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	entries := h.hub.GetScratchpad(id)
	writeJSON(w, http.StatusOK, entries)
}

func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err)
		return
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		slog.Error("failed to read auth message", "error", err)
		conn.Close()
		return
	}

	var env protocol.Envelope
	if err := json.Unmarshal(msg, &env); err != nil || env.Type != protocol.TypeAuth {
		sendWSMessage(conn, protocol.NewError("", "first message must be auth"))
		conn.Close()
		return
	}

	var authPayload protocol.AuthPayload
	if err := json.Unmarshal(env.Payload, &authPayload); err != nil {
		sendWSMessage(conn, protocol.NewError("", "invalid auth payload"))
		conn.Close()
		return
	}

	if authPayload.AgentID == "" {
		sendWSMessage(conn, protocol.NewError("", "agent_id is required"))
		conn.Close()
		return
	}

	sess, ok := h.sessionStore.ValidatePSK(authPayload.SessionID, authPayload.PSK)
	if !ok {
		sendWSMessage(conn, protocol.NewError(authPayload.SessionID, "invalid session or PSK"))
		conn.Close()
		return
	}

	conn.SetReadDeadline(time.Time{})

	leaderID := h.hub.GetLeader(sess.ID)
	sendWSMessage(conn, protocol.Envelope{
		Type:      protocol.TypeAuthOK,
		SessionID: sess.ID,
		From:      "server",
		To:        authPayload.AgentID,
		Payload: mustMarshal(map[string]any{
			"leader_id": leaderID,
		}),
		Timestamp: time.Now().UTC(),
	})

	agentName := authPayload.AgentName
	if agentName == "" {
		agentName = authPayload.AgentID
	}

	h.hub.Register(conn, sess.ID, authPayload.AgentID, agentName, authPayload.Capabilities)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func sendWSMessage(conn *websocket.Conn, env protocol.Envelope) {
	data, _ := json.Marshal(env)
	conn.WriteMessage(websocket.TextMessage, data)
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
