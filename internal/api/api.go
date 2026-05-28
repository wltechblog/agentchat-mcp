package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/wltechblog/agentchat-mcp/internal/hub"
	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

const maxUploadMemory = 50 << 20

type contextKey string

const (
	ctxKeyAgentID  contextKey = "agent_id"
	ctxKeySession  contextKey = "session"
	ctxKeyAgentKey contextKey = "agent_key"
)

type Handler struct {
	hub          *hub.Hub
	sessionStore *session.Store
}

func New(h *hub.Hub, store *session.Store) *Handler {
	return &Handler{
		hub:          h,
		sessionStore: store,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sessions", h.createSession)
	mux.HandleFunc("GET /sessions", h.listSessions)
	mux.HandleFunc("GET /sessions/{id}", h.getSession)
	mux.HandleFunc("DELETE /sessions/{id}", h.deleteSession)

	mux.HandleFunc("POST /sessions/{id}/register", h.registerAgent)
	mux.HandleFunc("POST /sessions/{id}/messages", h.auth(h.sendMessage))
	mux.HandleFunc("POST /sessions/{id}/broadcast", h.auth(h.broadcastMessage))
	mux.HandleFunc("GET /sessions/{id}/mailbox", h.auth(h.drainMailbox))
	mux.HandleFunc("GET /sessions/{id}/agents", h.auth(h.listAgents))
	mux.HandleFunc("GET /sessions/{id}/leader", h.auth(h.getLeader))
	mux.HandleFunc("POST /sessions/{id}/leader/transfer", h.auth(h.transferLeader))
	mux.HandleFunc("GET /sessions/{id}/history", h.auth(h.getHistory))
	mux.HandleFunc("GET /sessions/{id}/scratchpad", h.auth(h.getScratchpad))
	mux.HandleFunc("POST /sessions/{id}/scratchpad/set", h.auth(h.scratchpadSet))
	mux.HandleFunc("POST /sessions/{id}/scratchpad/get", h.auth(h.scratchpadGet))
	mux.HandleFunc("POST /sessions/{id}/scratchpad/delete", h.auth(h.scratchpadDelete))
	mux.HandleFunc("POST /sessions/{id}/files", h.auth(h.uploadFile))
	mux.HandleFunc("GET /sessions/{id}/files", h.auth(h.listFiles))
	mux.HandleFunc("GET /sessions/{id}/files/{fileID}", h.auth(h.downloadFile))
	mux.HandleFunc("DELETE /sessions/{id}/files/{fileID}", h.auth(h.deleteFile))
}

func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if sessionID == "" {
			http.Error(w, "session id required", http.StatusBadRequest)
			return
		}

		authHeader := r.Header.Get("Authorization")
		psk := strings.TrimPrefix(authHeader, "Bearer ")
		if psk == authHeader || psk == "" {
			http.Error(w, "Authorization: Bearer <psk> required", http.StatusUnauthorized)
			return
		}

		agentID := r.Header.Get("X-Agent-ID")
		if agentID == "" {
			http.Error(w, "X-Agent-ID header required", http.StatusUnauthorized)
			return
		}

		sess, ok := h.sessionStore.ValidatePSK(sessionID, psk)
		if !ok {
			http.Error(w, "invalid session or PSK", http.StatusUnauthorized)
			return
		}

		h.hub.RefreshPresence(sessionID, agentID)

		ctx := context.WithValue(r.Context(), ctxKeyAgentID, agentID)
		ctx = context.WithValue(ctx, ctxKeySession, sess)
		next(w, r.WithContext(ctx))
	}
}

func getAgentID(r *http.Request) string {
	v, _ := r.Context().Value(ctxKeyAgentID).(string)
	return v
}

func getSessionID(r *http.Request) string {
	return r.PathValue("id")
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

func (h *Handler) registerAgent(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	authHeader := r.Header.Get("Authorization")
	psk := strings.TrimPrefix(authHeader, "Bearer ")
	if psk == authHeader || psk == "" {
		http.Error(w, "Authorization: Bearer <psk> required", http.StatusUnauthorized)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusUnauthorized)
		return
	}

	sess, created, err := h.sessionStore.GetOrCreate(sessionID, psk, "")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sess == nil {
		http.Error(w, "invalid PSK", http.StatusUnauthorized)
		return
	}
	if created {
		slog.Info("session auto-created", "id", sess.ID, "name", sess.Name)
	}

	var req struct {
		AgentName    string   `json:"agent_name"`
		Capabilities []string `json:"capabilities"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	agentName := req.AgentName
	if agentName == "" {
		agentName = agentID
	}
	caps := req.Capabilities
	if caps == nil {
		caps = []string{}
	}

	h.hub.Register(sessionID, agentID, agentName, caps)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "registered",
		"leader_id": h.hub.GetLeader(sessionID),
		"agents":    h.hub.GetSessionAgents(sessionID),
	})
}

func (h *Handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agentID := getAgentID(r)

	var req struct {
		To      string          `json:"to"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.To == "" {
		http.Error(w, "'to' is required", http.StatusBadRequest)
		return
	}

	msgType := req.Type
	if msgType == "" {
		msgType = protocol.TypeMessage
	}

	if err := h.hub.SendMessage(sessionID, agentID, req.To, msgType, req.Payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (h *Handler) broadcastMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agentID := getAgentID(r)

	var req struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	msgType := req.Type
	if msgType == "" {
		msgType = protocol.TypeBroadcast
	}

	h.hub.Broadcast(sessionID, agentID, msgType, req.Payload)
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (h *Handler) drainMailbox(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agentID := getAgentID(r)

	entries := h.hub.DrainMailbox(sessionID, agentID)
	if entries == nil {
		entries = []mailbox.Entry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": entries,
		"count":    len(entries),
	})
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agents := h.hub.GetSessionAgents(sessionID)
	if agents == nil {
		agents = []protocol.AgentInfo{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func (h *Handler) getLeader(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	leaderID := h.hub.GetLeader(sessionID)
	writeJSON(w, http.StatusOK, map[string]string{"leader_id": leaderID})
}

func (h *Handler) transferLeader(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agentID := getAgentID(r)

	var req struct {
		NewLeaderID string `json:"new_leader_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.hub.LeaderTransfer(sessionID, agentID, req.NewLeaderID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "transferred", "leader_id": req.NewLeaderID})
}

func (h *Handler) getHistory(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)

	afterSeq, _ := strconv.ParseInt(r.URL.Query().Get("after_sequence"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	if afterSeq > 0 || limit > 0 {
		history := h.hub.GetHistoryAfter(sessionID, afterSeq, limit)
		writeJSON(w, http.StatusOK, history)
		return
	}

	history := h.hub.GetHistory(sessionID)
	writeJSON(w, http.StatusOK, history)
}

func (h *Handler) getScratchpad(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	entries := h.hub.GetScratchpad(sessionID)
	writeJSON(w, http.StatusOK, entries)
}

func (h *Handler) scratchpadSet(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agentID := getAgentID(r)

	var req struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	entry, err := h.hub.ScratchpadSet(sessionID, agentID, req.Key, req.Value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *Handler) scratchpadGet(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)

	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	entry, err := h.hub.ScratchpadGet(sessionID, req.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *Handler) scratchpadDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agentID := getAgentID(r)

	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	if err := h.hub.ScratchpadDelete(sessionID, agentID, req.Key); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": req.Key, "deleted": "true"})
}

func (h *Handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	sessionID := getSessionID(r)
	agentID := getAgentID(r)

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

	f, err := h.hub.StoreFile(sessionID, filename, contentType, agentID, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	slog.Info("file uploaded", "session", sessionID, "file", f.Name, "file_id", f.ID, "size", f.Size, "agent", agentID)
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
	sessionID := getSessionID(r)
	files := h.hub.GetFiles(sessionID)
	writeJSON(w, http.StatusOK, files)
}

func (h *Handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	fileID := r.PathValue("fileID")
	f, ok := h.hub.GetFile(sessionID, fileID)
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
	sessionID := r.PathValue("id")
	fileID := r.PathValue("fileID")
	if !h.hub.DeleteFile(sessionID, fileID) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	slog.Info("file deleted", "session", sessionID, "file_id", fileID)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
