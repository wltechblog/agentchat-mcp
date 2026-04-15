package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wltechblog/agentchat-mcp/internal/hub"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

func setupTestServer(t *testing.T) (*httptest.Server, *session.Store) {
	t.Helper()
	return setupTestServerWithGrace(t, 30*time.Second)
}

func setupTestServerWithGrace(t *testing.T, grace time.Duration) (*httptest.Server, *session.Store) {
	t.Helper()
	store := session.NewStore()
	lt := leader.NewTracker()
	sp := scratchpad.NewStore()
	h := hub.New(store, lt, sp, hub.WithGracePeriod(grace))
	handler := New(h, store, lt, sp)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, store
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func createTestSession(t *testing.T, server *httptest.Server) (string, string) {
	t.Helper()
	resp, err := http.Post(server.URL+"/sessions", "application/json", strings.NewReader(`{"name":"test"}`))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID  string `json:"id"`
		PSK string `json:"psk"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.ID, result.PSK
}

func authAgent(t *testing.T, ws *websocket.Conn, sessionID, agentID, psk string) {
	t.Helper()
	authAgentWithCapabilities(t, ws, sessionID, agentID, psk, nil)
}

func authAgentWithCapabilities(t *testing.T, ws *websocket.Conn, sessionID, agentID, psk string, capabilities []string) {
	t.Helper()
	authPayload, _ := json.Marshal(protocol.AuthPayload{
		SessionID:    sessionID,
		AgentID:      agentID,
		AgentName:    agentID,
		PSK:          psk,
		Capabilities: capabilities,
	})
	env := protocol.Envelope{
		Type:      protocol.TypeAuth,
		Payload:   authPayload,
		Timestamp: time.Now().UTC(),
	}
	data, _ := json.Marshal(env)
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	_, resp, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	var authResp protocol.Envelope
	json.Unmarshal(resp, &authResp)
	if authResp.Type != protocol.TypeAuthOK {
		t.Fatalf("expected auth_ok, got %s: %s", authResp.Type, string(authResp.Payload))
	}
}

func readEnvelope(t *testing.T, ws *websocket.Conn) protocol.Envelope {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	var env protocol.Envelope
	json.Unmarshal(data, &env)
	return env
}

func tryReadEnvelope(ws *websocket.Conn) (protocol.Envelope, bool) {
	ws.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return protocol.Envelope{}, false
	}
	var env protocol.Envelope
	json.Unmarshal(data, &env)
	return env, true
}

func writeEnvelope(t *testing.T, ws *websocket.Conn, env protocol.Envelope) {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
}

func TestWebSocketAuth(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	ws := dialWS(t, wsURL)
	authAgent(t, ws, sessionID, "agent-1", psk)
}

func TestWebSocketAuthBadPSK(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, _ := createTestSession(t, server)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	ws := dialWS(t, wsURL)

	authPayload, _ := json.Marshal(protocol.AuthPayload{
		SessionID: sessionID,
		AgentID:   "agent-1",
		PSK:       "wrong-psk",
	})
	env := protocol.Envelope{
		Type:    protocol.TypeAuth,
		Payload: authPayload,
	}
	data, _ := json.Marshal(env)
	ws.WriteMessage(websocket.TextMessage, data)

	_, resp, _ := ws.ReadMessage()
	var errResp protocol.Envelope
	json.Unmarshal(resp, &errResp)
	if errResp.Type != protocol.TypeError {
		t.Fatalf("expected error, got %s", errResp.Type)
	}
}

func TestAgentJoinedBroadcast(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)

	ws2 := dialWS(t, wsURL)
	authAgent(t, ws2, sessionID, "agent-2", psk)

	notification := readEnvelope(t, ws1)
	if notification.Type != protocol.TypeAgentJoined {
		t.Fatalf("expected agent_joined, got %s", notification.Type)
	}
	var info protocol.AgentInfo
	json.Unmarshal(notification.Payload, &info)
	if info.AgentID != "agent-2" {
		t.Fatalf("expected agent-2, got %s", info.AgentID)
	}
}

func TestDirectMessage(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	msgPayload, _ := json.Marshal(map[string]string{"text": "hello agent-2"})
	env := protocol.Envelope{
		Type:    protocol.TypeMessage,
		To:      "agent-2",
		Payload: msgPayload,
	}
	writeEnvelope(t, ws1, env)

	received := readEnvelope(t, ws2)
	if received.Type != protocol.TypeMessage {
		t.Fatalf("expected message, got %s", received.Type)
	}
	if received.From != "agent-1" {
		t.Fatalf("expected from agent-1, got %s", received.From)
	}
	if received.Sequence == 0 {
		t.Fatal("expected non-zero sequence number")
	}
}

func TestBroadcast(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	ws3 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined
	authAgent(t, ws3, sessionID, "agent-3", psk)
	readEnvelope(t, ws1) // agent-3 joined
	readEnvelope(t, ws2) // agent-3 joined

	bcastPayload, _ := json.Marshal(map[string]string{"text": "hello all"})
	env := protocol.Envelope{
		Type:    protocol.TypeBroadcast,
		Payload: bcastPayload,
	}
	writeEnvelope(t, ws1, env)

	var wg sync.WaitGroup
	wg.Add(2)
	for _, ws := range []*websocket.Conn{ws2, ws3} {
		go func(w *websocket.Conn) {
			defer wg.Done()
			w.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, d, err := w.ReadMessage()
			if err != nil {
				t.Errorf("read broadcast: %v", err)
				return
			}
			var e protocol.Envelope
			json.Unmarshal(d, &e)
			if e.Type != protocol.TypeBroadcast {
				t.Errorf("expected broadcast, got %s", e.Type)
			}
			if e.From != "agent-1" {
				t.Errorf("expected from agent-1, got %s", e.From)
			}
		}(ws)
	}
	wg.Wait()
}

func TestAgentLeave(t *testing.T) {
	server, _ := setupTestServerWithGrace(t, 100*time.Millisecond)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	ws2.Close()

	notification := readEnvelope(t, ws1)
	if notification.Type != protocol.TypeAgentLeft {
		t.Fatalf("expected agent_left, got %s", notification.Type)
	}
}

func TestListAgents(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	env := protocol.Envelope{Type: protocol.TypeListAgents}
	writeEnvelope(t, ws1, env)

	resp := readEnvelope(t, ws1)
	if resp.Type != protocol.TypeAgentsList {
		t.Fatalf("expected agents_list, got %s", resp.Type)
	}
	var agents []protocol.AgentInfo
	json.Unmarshal(resp.Payload, &agents)
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
}

func TestCapabilities(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgentWithCapabilities(t, ws1, sessionID, "agent-1", psk, []string{"search", "analyze"})
	authAgentWithCapabilities(t, ws2, sessionID, "agent-2", psk, []string{"write"})
	readEnvelope(t, ws1) // agent-2 joined

	env := protocol.Envelope{Type: protocol.TypeListAgents}
	writeEnvelope(t, ws1, env)

	resp := readEnvelope(t, ws1)
	var agents []protocol.AgentInfo
	json.Unmarshal(resp.Payload, &agents)

	for _, a := range agents {
		if a.AgentID == "agent-1" {
			if len(a.Capabilities) != 2 || a.Capabilities[0] != "search" {
				t.Fatalf("expected agent-1 capabilities [search, analyze], got %v", a.Capabilities)
			}
		}
		if a.AgentID == "agent-2" {
			if len(a.Capabilities) != 1 || a.Capabilities[0] != "write" {
				t.Fatalf("expected agent-2 capabilities [write], got %v", a.Capabilities)
			}
		}
	}
}

func TestScratchpadSetGetDelete(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	// Set
	setPayload, _ := json.Marshal(protocol.ScratchpadSetPayload{
		Key:   "plan",
		Value: json.RawMessage(`"step 1"`),
	})
	writeEnvelope(t, ws1, protocol.Envelope{
		Type:    protocol.TypeScratchpadSet,
		Payload: setPayload,
	})

	// Agent-1 gets scratchpad_result
	result := readEnvelope(t, ws1)
	if result.Type != protocol.TypeScratchpadResult {
		t.Fatalf("expected scratchpad_result, got %s", result.Type)
	}

	// Agent-2 gets scratchpad_update broadcast
	update := readEnvelope(t, ws2)
	if update.Type != protocol.TypeScratchpadUpdate {
		t.Fatalf("expected scratchpad_update, got %s", update.Type)
	}

	// Get
	getPayload, _ := json.Marshal(map[string]string{"key": "plan"})
	writeEnvelope(t, ws1, protocol.Envelope{
		Type:    protocol.TypeScratchpadGet,
		Payload: getPayload,
	})
	result = readEnvelope(t, ws1)
	if result.Type != protocol.TypeScratchpadResult {
		t.Fatalf("expected scratchpad_result, got %s", result.Type)
	}
	var entry protocol.ScratchpadEntry
	json.Unmarshal(result.Payload, &entry)
	if entry.Key != "plan" {
		t.Fatalf("expected key 'plan', got %s", entry.Key)
	}
	if string(entry.Value) != `"step 1"` {
		t.Fatalf("expected value 'step 1', got %s", string(entry.Value))
	}

	// Delete
	delPayload, _ := json.Marshal(map[string]string{"key": "plan"})
	writeEnvelope(t, ws1, protocol.Envelope{
		Type:    protocol.TypeScratchpadDelete,
		Payload: delPayload,
	})
	result = readEnvelope(t, ws1)
	if result.Type != protocol.TypeScratchpadResult {
		t.Fatalf("expected scratchpad_result on delete, got %s", result.Type)
	}

	// Verify deleted
	writeEnvelope(t, ws1, protocol.Envelope{
		Type:    protocol.TypeScratchpadGet,
		Payload: getPayload,
	})
	result = readEnvelope(t, ws1)
	if result.Type != protocol.TypeError {
		t.Fatalf("expected error for deleted key, got %s", result.Type)
	}
}

func TestScratchpadList(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)

	for i := 0; i < 3; i++ {
		payload, _ := json.Marshal(protocol.ScratchpadSetPayload{
			Key:   "key" + string(rune('0'+i)),
			Value: json.RawMessage(`"val"`),
		})
		writeEnvelope(t, ws1, protocol.Envelope{Type: protocol.TypeScratchpadSet, Payload: payload})
		readEnvelope(t, ws1) // scratchpad_result
	}

	writeEnvelope(t, ws1, protocol.Envelope{Type: protocol.TypeScratchpadList})
	result := readEnvelope(t, ws1)
	if result.Type != protocol.TypeScratchpadResult {
		t.Fatalf("expected scratchpad_result, got %s", result.Type)
	}
	var listResult protocol.ScratchpadListResult
	json.Unmarshal(result.Payload, &listResult)
	if len(listResult.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(listResult.Entries))
	}
}

func TestLeaderInitialAndQuery(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)

	writeEnvelope(t, ws1, protocol.Envelope{Type: protocol.TypeLeaderQuery})
	resp := readEnvelope(t, ws1)
	if resp.Type != protocol.TypeLeaderInfo {
		t.Fatalf("expected leader_info, got %s", resp.Type)
	}
	var info map[string]string
	json.Unmarshal(resp.Payload, &info)
	if info["leader_id"] != "agent-1" {
		t.Fatalf("expected leader agent-1, got %s", info["leader_id"])
	}
}

func TestLeaderTransfer(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	transferPayload, _ := json.Marshal(protocol.LeaderTransferPayload{NewLeaderID: "agent-2"})
	writeEnvelope(t, ws1, protocol.Envelope{
		Type:    protocol.TypeLeaderTransfer,
		Payload: transferPayload,
	})

	// Both should get leader_info broadcast
	notification := readEnvelope(t, ws1)
	if notification.Type != protocol.TypeLeaderInfo {
		t.Fatalf("expected leader_info, got %s", notification.Type)
	}

	// Verify via query
	writeEnvelope(t, ws1, protocol.Envelope{Type: protocol.TypeLeaderQuery})
	resp := readEnvelope(t, ws1)
	var info map[string]string
	json.Unmarshal(resp.Payload, &info)
	if info["leader_id"] != "agent-2" {
		t.Fatalf("expected leader agent-2, got %s", info["leader_id"])
	}
}

func TestLeaderTransferUnauthorized(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	// agent-2 tries to transfer (but agent-1 is leader)
	transferPayload, _ := json.Marshal(protocol.LeaderTransferPayload{NewLeaderID: "agent-2"})
	writeEnvelope(t, ws2, protocol.Envelope{
		Type:    protocol.TypeLeaderTransfer,
		Payload: transferPayload,
	})

	resp := readEnvelope(t, ws2)
	if resp.Type != protocol.TypeError {
		t.Fatalf("expected error, got %s", resp.Type)
	}
}

func TestLeaderAutoTransferOnLeave(t *testing.T) {
	server, _ := setupTestServerWithGrace(t, 100*time.Millisecond)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	// agent-1 (leader) disconnects
	ws1.Close()

	// ws2 should get agent_left then leader_info
	for i := 0; i < 2; i++ {
		env := readEnvelope(t, ws2)
		if env.Type == protocol.TypeLeaderInfo {
			var info map[string]string
			json.Unmarshal(env.Payload, &info)
			if info["leader_id"] != "agent-2" {
				t.Fatalf("expected auto-transfer to agent-2, got %s", info["leader_id"])
			}
			return
		}
	}
	t.Fatal("expected leader_info broadcast after leader left")
}

func TestHistoryRequest(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	// Send 3 messages
	for i := 0; i < 3; i++ {
		payload, _ := json.Marshal(map[string]string{"text": "msg"})
		writeEnvelope(t, ws1, protocol.Envelope{
			Type:    protocol.TypeBroadcast,
			Payload: payload,
		})
	}
	// drain broadcast from ws2
	for i := 0; i < 3; i++ {
		readEnvelope(t, ws2)
	}

	// Request all history
	histPayload, _ := json.Marshal(protocol.HistoryRequestPayload{})
	writeEnvelope(t, ws1, protocol.Envelope{
		Type:    protocol.TypeHistoryRequest,
		Payload: histPayload,
	})
	resp := readEnvelope(t, ws1)
	if resp.Type != protocol.TypeHistoryResult {
		t.Fatalf("expected history_result, got %s", resp.Type)
	}
	var histData map[string]any
	json.Unmarshal(resp.Payload, &histData)
	count, _ := histData["count"].(float64)
	if int(count) != 3 {
		t.Fatalf("expected 3 history messages, got %v", count)
	}
}

func TestHistoryRequestAfterSequence(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	ws2 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)
	authAgent(t, ws2, sessionID, "agent-2", psk)
	readEnvelope(t, ws1) // agent-2 joined

	// Send 5 broadcasts
	for i := 0; i < 5; i++ {
		payload, _ := json.Marshal(map[string]string{"text": "msg"})
		writeEnvelope(t, ws1, protocol.Envelope{
			Type:    protocol.TypeBroadcast,
			Payload: payload,
		})
	}
	for i := 0; i < 5; i++ {
		readEnvelope(t, ws2) // drain
	}

	// Request only messages after sequence 2
	histPayload, _ := json.Marshal(protocol.HistoryRequestPayload{AfterSequence: 2})
	writeEnvelope(t, ws1, protocol.Envelope{
		Type:    protocol.TypeHistoryRequest,
		Payload: histPayload,
	})
	resp := readEnvelope(t, ws1)
	var histData map[string]any
	json.Unmarshal(resp.Payload, &histData)
	count, _ := histData["count"].(float64)
	if int(count) != 3 {
		t.Fatalf("expected 3 messages after seq 2, got %v", count)
	}
}

func TestRESTGetLeader(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)

	resp, err := http.Get(server.URL + "/sessions/" + sessionID + "/leader")
	if err != nil {
		t.Fatalf("get leader: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["leader_id"] != "agent-1" {
		t.Fatalf("expected leader agent-1, got %s", result["leader_id"])
	}
}

func TestRESTGetScratchpad(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)

	// Set a scratchpad key
	setPayload, _ := json.Marshal(protocol.ScratchpadSetPayload{
		Key:   "test-key",
		Value: json.RawMessage(`"test-value"`),
	})
	writeEnvelope(t, ws1, protocol.Envelope{Type: protocol.TypeScratchpadSet, Payload: setPayload})
	readEnvelope(t, ws1) // scratchpad_result

	// REST get scratchpad
	resp, err := http.Get(server.URL + "/sessions/" + sessionID + "/scratchpad")
	if err != nil {
		t.Fatalf("get scratchpad: %v", err)
	}
	defer resp.Body.Close()

	var entries []protocol.ScratchpadEntry
	json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) != 1 || entries[0].Key != "test-key" {
		t.Fatalf("expected 1 entry with key 'test-key', got %v", entries)
	}
}

func TestDuplicateAgentIDRejected(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws1 := dialWS(t, wsURL)
	authAgent(t, ws1, sessionID, "agent-1", psk)

	ws2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws2: %v", err)
	}
	defer ws2.Close()

	authPayload, _ := json.Marshal(protocol.AuthPayload{
		SessionID: sessionID,
		AgentID:   "agent-1",
		PSK:       psk,
	})
	authMsg, _ := json.Marshal(protocol.Envelope{
		Type:      protocol.TypeAuth,
		Payload:   authPayload,
		Timestamp: time.Now().UTC(),
	})
	ws2.WriteMessage(websocket.TextMessage, authMsg)

	ws2.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, resp, err := ws2.ReadMessage()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var errResp protocol.Envelope
	json.Unmarshal(resp, &errResp)
	if errResp.Type != protocol.TypeError {
		t.Fatalf("expected error for duplicate agent_id, got %s: %s", errResp.Type, string(resp))
	}
}
