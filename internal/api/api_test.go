package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/filestore"
	"github.com/wltechblog/agentchat-mcp/internal/hub"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/presence"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

func setupTestServer(t *testing.T) (*httptest.Server, *session.Store) {
	t.Helper()
	store := session.NewStore()
	lt := leader.NewTracker()
	sp := scratchpad.NewStore()
	fs := filestore.NewStore(10 << 20)
	pt := presence.NewTracker(60 * time.Second)
	mb := mailbox.NewStore(1000)
	h := hub.New(store, lt, sp, fs, pt, mb)
	handler := New(h, store)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		server.Close()
		pt.Stop()
	})
	return server, store
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

func doAuthRequest(t *testing.T, server, method, path, sessionID, psk, agentID string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, server+path, bodyReader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+psk)
	req.Header.Set("X-Agent-ID", agentID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func registerAgent(t *testing.T, server, sessionID, psk, agentID string, caps []string) {
	t.Helper()
	body := map[string]any{
		"agent_name":    agentID,
		"capabilities":  caps,
	}
	resp := doAuthRequest(t, server, "POST", "/sessions/"+sessionID+"/register", sessionID, psk, agentID, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rbody, _ := io.ReadAll(resp.Body)
		t.Fatalf("register agent %s failed (%d): %s", agentID, resp.StatusCode, string(rbody))
	}
}

func drainMailbox(t *testing.T, server, sessionID, psk, agentID string) []map[string]any {
	t.Helper()
	resp := doAuthRequest(t, server, "GET", "/sessions/"+sessionID+"/mailbox", sessionID, psk, agentID, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rbody, _ := io.ReadAll(resp.Body)
		t.Fatalf("drain mailbox failed (%d): %s", resp.StatusCode, string(rbody))
	}
	var result struct {
		Messages []map[string]any `json:"messages"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Messages
}

func TestRegisterAgent(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/register", sessionID, psk, "agent-1", map[string]any{
		"agent_name":   "Agent One",
		"capabilities": []string{"search"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["leader_id"] != "agent-1" {
		t.Fatalf("expected leader_id agent-1, got %v", result["leader_id"])
	}
}

func TestAuthBadPSK(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, _ := createTestSession(t, server)

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/mailbox", sessionID, "wrong-psk", "agent-1", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuthMissingHeaders(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	req, _ := http.NewRequest("GET", server.URL+"/sessions/"+sessionID+"/mailbox", nil)
	req.Header.Set("Authorization", "Bearer "+psk)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Agent-ID, got %d", resp.StatusCode)
	}
}

func TestDirectMessageViaMailbox(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)

	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/messages", sessionID, psk, "agent-1", map[string]any{
		"to":      "agent-2",
		"type":    "message",
		"payload": map[string]string{"text": "hello"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	msgs := drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	env := msgs[0]["envelope"].(map[string]any)
	if env["type"] != "message" {
		t.Fatalf("expected type message, got %v", env["type"])
	}
	if env["from"] != "agent-1" {
		t.Fatalf("expected from agent-1, got %v", env["from"])
	}
}

func TestDirectMessageMissingTo(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/messages", sessionID, psk, "agent-1", map[string]any{
		"type":    "message",
		"payload": map[string]string{"text": "hello"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestBroadcastViaMailbox(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-3", nil)

	drainMailbox(t, server.URL, sessionID, psk, "agent-1")
	drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	drainMailbox(t, server.URL, sessionID, psk, "agent-3")

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/broadcast", sessionID, psk, "agent-1", map[string]any{
		"type":    "broadcast",
		"payload": map[string]string{"text": "hello all"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	msgs2 := drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	msgs3 := drainMailbox(t, server.URL, sessionID, psk, "agent-3")

	if len(msgs2) != 1 {
		t.Fatalf("expected 1 broadcast for agent-2, got %d", len(msgs2))
	}
	if len(msgs3) != 1 {
		t.Fatalf("expected 1 broadcast for agent-3, got %d", len(msgs3))
	}

	msgs1 := drainMailbox(t, server.URL, sessionID, psk, "agent-1")
	if len(msgs1) != 0 {
		t.Fatalf("expected 0 messages for sender, got %d", len(msgs1))
	}
}

func TestMailboxDestructiveRead(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)
	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/messages", sessionID, psk, "agent-1", map[string]any{
		"to": "agent-2", "type": "message", "payload": map[string]string{"text": "msg1"},
	}).Body.Close()
	doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/messages", sessionID, psk, "agent-1", map[string]any{
		"to": "agent-2", "type": "message", "payload": map[string]string{"text": "msg2"},
	}).Body.Close()

	msgs := drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	msgs2 := drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	if len(msgs2) != 0 {
		t.Fatalf("expected 0 after drain, got %d", len(msgs2))
	}
}

func TestListAgents(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	registerAgent(t, server.URL, sessionID, psk, "agent-1", []string{"search"})
	registerAgent(t, server.URL, sessionID, psk, "agent-2", []string{"write"})

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/agents", sessionID, psk, "agent-1", nil)
	defer resp.Body.Close()

	var agents []protocol.AgentInfo
	json.NewDecoder(resp.Body).Decode(&agents)
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
}

func TestCapabilities(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	registerAgent(t, server.URL, sessionID, psk, "agent-1", []string{"search", "analyze"})
	registerAgent(t, server.URL, sessionID, psk, "agent-2", []string{"write"})

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/agents", sessionID, psk, "agent-1", nil)
	defer resp.Body.Close()

	var agents []protocol.AgentInfo
	json.NewDecoder(resp.Body).Decode(&agents)

	for _, a := range agents {
		if a.AgentID == "agent-1" {
			if len(a.Capabilities) != 2 || a.Capabilities[0] != "search" {
				t.Fatalf("expected [search, analyze], got %v", a.Capabilities)
			}
		}
		if a.AgentID == "agent-2" {
			if len(a.Capabilities) != 1 || a.Capabilities[0] != "write" {
				t.Fatalf("expected [write], got %v", a.Capabilities)
			}
		}
	}
}

func TestScratchpadSetGetDelete(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/scratchpad/set", sessionID, psk, "agent-1", map[string]any{
		"key":   "plan",
		"value": "step 1",
	})
	defer resp.Body.Close()

	var entry protocol.ScratchpadEntry
	json.NewDecoder(resp.Body).Decode(&entry)
	if entry.Key != "plan" {
		t.Fatalf("expected key 'plan', got %s", entry.Key)
	}

	resp2 := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/scratchpad/get", sessionID, psk, "agent-1", map[string]any{
		"key": "plan",
	})
	defer resp2.Body.Close()

	var entry2 protocol.ScratchpadEntry
	json.NewDecoder(resp2.Body).Decode(&entry2)
	if entry2.Key != "plan" {
		t.Fatalf("expected key 'plan', got %s", entry2.Key)
	}

	resp3 := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/scratchpad/delete", sessionID, psk, "agent-1", map[string]any{
		"key": "plan",
	})
	defer resp3.Body.Close()

	resp4 := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/scratchpad/get", sessionID, psk, "agent-1", map[string]any{
		"key": "plan",
	})
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for deleted key, got %d", resp4.StatusCode)
	}
}

func TestScratchpadList(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	for i := 0; i < 3; i++ {
		doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/scratchpad/set", sessionID, psk, "agent-1", map[string]any{
			"key":   "key" + string(rune('0'+i)),
			"value": "val",
		}).Body.Close()
	}

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/scratchpad", sessionID, psk, "agent-1", nil)
	defer resp.Body.Close()

	var entries []protocol.ScratchpadEntry
	json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
}

func TestLeaderInitialAndQuery(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/leader", sessionID, psk, "agent-1", nil)
	defer resp.Body.Close()

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["leader_id"] != "agent-1" {
		t.Fatalf("expected leader agent-1, got %s", result["leader_id"])
	}
}

func TestLeaderTransfer(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/leader/transfer", sessionID, psk, "agent-1", map[string]any{
		"new_leader_id": "agent-2",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	resp2 := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/leader", sessionID, psk, "agent-1", nil)
	defer resp2.Body.Close()
	var result map[string]string
	json.NewDecoder(resp2.Body).Decode(&result)
	if result["leader_id"] != "agent-2" {
		t.Fatalf("expected leader agent-2, got %s", result["leader_id"])
	}
}

func TestLeaderTransferUnauthorized(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/leader/transfer", sessionID, psk, "agent-2", map[string]any{
		"new_leader_id": "agent-2",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHistoryRequest(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)

	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	for i := 0; i < 3; i++ {
		doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/broadcast", sessionID, psk, "agent-1", map[string]any{
			"type":    "broadcast",
			"payload": map[string]string{"text": "msg"},
		}).Body.Close()
	}

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/history", sessionID, psk, "agent-1", nil)
	defer resp.Body.Close()

	var history []protocol.Envelope
	json.NewDecoder(resp.Body).Decode(&history)
	if len(history) != 3 {
		t.Fatalf("expected 3 history messages, got %d", len(history))
	}
}

func TestHistoryRequestAfterSequence(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)

	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	for i := 0; i < 5; i++ {
		doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/broadcast", sessionID, psk, "agent-1", map[string]any{
			"type":    "broadcast",
			"payload": map[string]string{"text": "msg"},
		}).Body.Close()
	}

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/history?after_sequence=2", sessionID, psk, "agent-1", nil)
	defer resp.Body.Close()

	var history []protocol.Envelope
	json.NewDecoder(resp.Body).Decode(&history)
	if len(history) != 3 {
		t.Fatalf("expected 3 messages after seq 2, got %d", len(history))
	}
}

func TestFileUploadAndDownload(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	fileContent := []byte("hello from agent-1, this is a test file!")
	req, _ := http.NewRequest("POST", server.URL+"/sessions/"+sessionID+"/files?filename=test.txt", bytes.NewReader(fileContent))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+psk)
	req.Header.Set("X-Agent-ID", "agent-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, string(body))
	}

	var uploadResp map[string]any
	json.NewDecoder(resp.Body).Decode(&uploadResp)
	fileID, _ := uploadResp["file_id"].(string)
	if fileID == "" {
		t.Fatal("expected file_id in response")
	}

	req2, _ := http.NewRequest("GET", server.URL+"/sessions/"+sessionID+"/files/"+fileID, nil)
	req2.Header.Set("Authorization", "Bearer "+psk)
	req2.Header.Set("X-Agent-ID", "agent-1")
	dlResp, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("download file: %v", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", dlResp.StatusCode)
	}
	downloaded, _ := io.ReadAll(dlResp.Body)
	if string(downloaded) != string(fileContent) {
		t.Fatalf("file content mismatch: got %q", string(downloaded))
	}
}

func TestFileShareViaMailbox(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)
	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	fileContent := "shared data payload"
	req, _ := http.NewRequest("POST", server.URL+"/sessions/"+sessionID+"/files?filename=report.csv", strings.NewReader(fileContent))
	req.Header.Set("Content-Type", "text/csv")
	req.Header.Set("Authorization", "Bearer "+psk)
	req.Header.Set("X-Agent-ID", "agent-1")
	uploadResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	var uploadResult map[string]any
	json.NewDecoder(uploadResp.Body).Decode(&uploadResult)
	uploadResp.Body.Close()
	fileID := uploadResult["file_id"].(string)

	sharePayload, _ := json.Marshal(protocol.FileSharePayload{
		FileID: fileID, FileName: "report.csv", ContentType: "text/csv",
		Size: int64(len(fileContent)), Description: "Monthly report",
	})
	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/messages", sessionID, psk, "agent-1", map[string]any{
		"to":      "agent-2",
		"type":    "file_share",
		"payload": json.RawMessage(sharePayload),
	})
	defer resp.Body.Close()

	msgs := drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	env := msgs[0]["envelope"].(map[string]any)
	if env["type"] != "file_share" {
		t.Fatalf("expected file_share, got %v", env["type"])
	}
	if env["from"] != "agent-1" {
		t.Fatalf("expected from agent-1, got %v", env["from"])
	}
}

func TestFileDelete(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	req, _ := http.NewRequest("POST", server.URL+"/sessions/"+sessionID+"/files?filename=del.txt", strings.NewReader("bye"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+psk)
	req.Header.Set("X-Agent-ID", "agent-1")
	resp, _ := http.DefaultClient.Do(req)
	var upload map[string]any
	json.NewDecoder(resp.Body).Decode(&upload)
	resp.Body.Close()
	fileID := upload["file_id"].(string)

	delReq, _ := http.NewRequest("DELETE", server.URL+"/sessions/"+sessionID+"/files/"+fileID, nil)
	delReq.Header.Set("Authorization", "Bearer "+psk)
	delReq.Header.Set("X-Agent-ID", "agent-1")
	delResp, _ := http.DefaultClient.Do(delReq)
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", delResp.StatusCode)
	}

	goneReq, _ := http.NewRequest("GET", server.URL+"/sessions/"+sessionID+"/files/"+fileID, nil)
	goneReq.Header.Set("Authorization", "Bearer "+psk)
	goneReq.Header.Set("X-Agent-ID", "agent-1")
	goneResp, _ := http.DefaultClient.Do(goneReq)
	if goneResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", goneResp.StatusCode)
	}
}

func TestAgentJoinedNotificationInMailbox(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)

	msgs := drainMailbox(t, server.URL, sessionID, psk, "agent-1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(msgs))
	}
	env := msgs[0]["envelope"].(map[string]any)
	if env["type"] != "agent_joined" {
		t.Fatalf("expected agent_joined, got %v", env["type"])
	}
}

func TestMultipleAgentsSameID(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	msgs := drainMailbox(t, server.URL, sessionID, psk, "agent-1")
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages for same-agent re-register, got %d", len(msgs))
	}
}

func TestScratchpadUpdateBroadcast(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)
	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/scratchpad/set", sessionID, psk, "agent-1", map[string]any{
		"key": "plan", "value": "step 1",
	}).Body.Close()

	msgs := drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 scratchpad_update, got %d", len(msgs))
	}
	env := msgs[0]["envelope"].(map[string]any)
	if env["type"] != "scratchpad_update" {
		t.Fatalf("expected scratchpad_update, got %v", env["type"])
	}
}
