package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wltechblog/agentchat-mcp/internal/filestore"
	"github.com/wltechblog/agentchat-mcp/internal/hub"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

const maxUploadMemory = 50 << 20

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
	files        *filestore.Store
}

func New(h *hub.Hub, store *session.Store, lt *leader.Tracker, sp *scratchpad.Store, fs *filestore.Store) *Handler {
	return &Handler{
		hub:          h,
		sessionStore: store,
		leader:       lt,
		scratchpad:   sp,
		files:        fs,
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
	mux.HandleFunc("POST /sessions/{id}/files", h.uploadFile)
	mux.HandleFunc("GET /sessions/{id}/files", h.listFiles)
	mux.HandleFunc("GET /sessions/{id}/files/{fileID}", h.downloadFile)
	mux.HandleFunc("DELETE /sessions/{id}/files/{fileID}", h.deleteFile)
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

func (h *Handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.sessionStore.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadMemory)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "file too large or read error", http.StatusRequestEntityTooLarge)
		return
	}

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		filename = r.URL.Query().Get("name")
	}
	if filename == "" {
		filename = "unnamed"
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	uploadedBy := r.Header.Get("X-Agent-ID")

	f, err := h.hub.StoreFile(id, filename, contentType, uploadedBy, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	slog.Info("file uploaded", "session", id, "file", f.Name, "file_id", f.ID, "size", f.Size, "agent", uploadedBy)
	writeJSON(w, http.StatusCreated, map[string]any{
		"file_id":      f.ID,
		"file_name":    f.Name,
		"content_type": f.ContentType,
		"size":         f.Size,
		"uploaded_by":  f.UploadedBy,
		"uploaded_at":  f.UploadedAt,
	})
}

func (h *Handler) listFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.sessionStore.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	files := h.hub.GetFiles(id)
	writeJSON(w, http.StatusOK, files)
}

func (h *Handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fileID := r.PathValue("fileID")
	f, ok := h.hub.GetFile(id, fileID)
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+f.Name+"\"")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Write(f.Data)
}

func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fileID := r.PathValue("fileID")
	if !h.hub.DeleteFile(id, fileID) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	slog.Info("file deleted", "session", id, "file_id", fileID)
	w.WriteHeader(http.StatusNoContent)
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

	if authPayload.SessionID == "" || authPayload.PSK == "" {
		sendWSMessage(conn, protocol.NewError("", "session_id and psk are required"))
		conn.Close()
		return
	}

	sess, created, err := h.sessionStore.GetOrCreate(authPayload.SessionID, authPayload.PSK, authPayload.SessionName)
	if err != nil {
		sendWSMessage(conn, protocol.NewError(authPayload.SessionID, "internal error"))
		conn.Close()
		return
	}
	if sess == nil {
		sendWSMessage(conn, protocol.NewError(authPayload.SessionID, "invalid PSK"))
		conn.Close()
		return
	}
	if created {
		slog.Info("session auto-created", "id", sess.ID, "name", sess.Name)
	}

	conn.SetReadDeadline(time.Time{})

	agentName := authPayload.AgentName
	if agentName == "" {
		agentName = authPayload.AgentID
	}

	ac, pending, err := h.hub.Register(conn, sess.ID, authPayload.AgentID, agentName, authPayload.Capabilities)
	if err != nil {
		sendWSMessage(conn, protocol.NewError(sess.ID, err.Error()))
		conn.Close()
		return
	}

	leaderID := h.hub.GetLeader(sess.ID)
	ac.Send(protocol.Envelope{
		Type:      protocol.TypeAuthOK,
		SessionID: sess.ID,
		From:      "server",
		To:        authPayload.AgentID,
		Payload: mustMarshal(map[string]any{
			"leader_id": leaderID,
		}),
		Timestamp: time.Now().UTC(),
	})

	for _, msg := range pending {
		ac.SendRaw(msg)
	}
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
