package api

import (
	"bufio"
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
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

// setupWithHandler is setupTestServer with the Handler exposed so tests can
// tune handler-level knobs.
func setupWithHandler(t *testing.T) (*httptest.Server, *Handler, *session.Store) {
	t.Helper()
	store := session.NewStore()
	pt := presence.NewTracker(60 * time.Second)
	h := hub.New(hub.Deps{SessionStore: store, Leader: leader.NewTracker(), Scratchpad: scratchpad.NewStore(),
		Files: filestore.NewStore(10 << 20), Presence: pt, Mailboxes: mailbox.NewStore(1000)})
	handler := New(h, store)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		server.Close()
		pt.Stop()
	})
	return server, handler, store
}

// TestWatchRejectsUnknownSession: /watch must never conjure a session into
// existence. Before the fix, any caller-supplied PSK auto-created any session
// ID — letting an attacker claim a live session's identity after a restart.
func TestWatchRejectsUnknownSession(t *testing.T) {
	server, _ := setupTestServer(t)

	resp, err := http.Get(server.URL + "/watch?session=doesnotexist&psk=garbage")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown session, got %d", resp.StatusCode)
	}
}

// TestMailboxRejectsUnknownSession: same guarantee on the authenticated API.
func TestMailboxRejectsUnknownSession(t *testing.T) {
	server, _ := setupTestServer(t)

	resp := doAuthRequest(t, server.URL, "GET", "/sessions/doesnotexist/mailbox", "doesnotexist", "garbage", "agent-1", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown session, got %d", resp.StatusCode)
	}
}

// TestWatchTokenFlow: register issues a short-lived watch token; /watch
// accepts it and rejects a tampered one, so clients can stop putting PSKs
// in URLs.
func TestWatchTokenFlow(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)

	resp := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/register", sessionID, psk, "agent-1", nil)
	defer resp.Body.Close()

	var reg struct {
		WatchToken string `json:"watch_token"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	if reg.WatchToken == "" {
		t.Fatal("expected watch_token in register response")
	}

	okResp, err := http.Get(server.URL + "/watch?session=" + sessionID + "&token=" + reg.WatchToken)
	if err != nil {
		t.Fatalf("watch with token: %v", err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for valid watch token, got %d", okResp.StatusCode)
	}

	badResp, err := http.Get(server.URL + "/watch?session=" + sessionID + "&token=" + reg.WatchToken + "xx")
	if err != nil {
		t.Fatalf("watch with bad token: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered token, got %d", badResp.StatusCode)
	}

	// A token is scoped to its session.
	otherResp, err := http.Get(server.URL + "/watch?session=other-session&token=" + reg.WatchToken)
	if err != nil {
		t.Fatalf("watch other session: %v", err)
	}
	otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for token used on another session, got %d", otherResp.StatusCode)
	}
}

// TestFilteredMailboxDrainLeavesNonMatching: a filtered drain takes only the
// matching messages; everything else stays queued.
func TestFilteredMailboxDrainLeavesNonMatching(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-3", nil)
	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	send := func(from string) {
		t.Helper()
		r := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/messages", sessionID, psk, from, map[string]any{
			"to": "agent-2", "type": "message", "payload": map[string]string{"text": "from " + from},
		})
		r.Body.Close()
	}
	send("agent-1")
	send("agent-3")

	// Drain only agent-1's message.
	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/mailbox?from=agent-1", sessionID, psk, "agent-2", nil)
	var filtered struct {
		Count int `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&filtered)
	resp.Body.Close()
	if filtered.Count != 1 {
		t.Fatalf("expected 1 filtered message, got %d", filtered.Count)
	}

	// agent-3's message must still be queued.
	remaining := drainMailbox(t, server.URL, sessionID, psk, "agent-2")
	if len(remaining) != 1 {
		t.Fatalf("expected non-matching message to survive, got %d", len(remaining))
	}
}

// TestMailboxLongPollReturnsWhenMessageArrives: the server holds the request
// and wakes as soon as a matching message is delivered.
func TestMailboxLongPollReturnsWhenMessageArrives(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)
	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)
	drainMailbox(t, server.URL, sessionID, psk, "agent-2")

	go func() {
		time.Sleep(400 * time.Millisecond)
		r := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/messages", sessionID, psk, "agent-1", map[string]any{
			"to": "agent-2", "type": "message", "payload": map[string]string{"text": "wake up"},
		})
		r.Body.Close()
	}()

	start := time.Now()
	resp := doAuthRequest(t, server.URL, "GET", "/sessions/"+sessionID+"/mailbox?wait=5&from=agent-1", sessionID, psk, "agent-2", nil)
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Count int `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Count != 1 {
		t.Fatalf("expected long-poll to return the message, got count %d", result.Count)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("long-poll returned only after %v; deliver signal not honored", elapsed)
	}
}

func TestHealthz(t *testing.T) {
	server, _ := setupTestServer(t)
	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("unexpected healthz body: %s", body)
	}
}

// TestWatchSubscriberCap: concurrent SSE streams per session are capped so a
// misbehaving client can't pin unbounded resources.
func TestWatchSubscriberCap(t *testing.T) {
	server, handler, store := setupWithHandler(t)
	handler.maxWatchersPerSession = 1

	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	first, err := http.Get(server.URL + "/watch?session=" + sessionID + "&psk=" + psk)
	if err != nil {
		t.Fatalf("first watch: %v", err)
	}
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for first watcher, got %d", first.StatusCode)
	}
	// Consume the handshake so the handler is past Subscribe.
	bufio.NewScanner(first.Body).Scan()

	// Give the server a moment to enter the handler loop.
	deadline := time.Now().Add(2 * time.Second)
	for handler.watcher.Count(sessionID) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	second, err := http.Get(server.URL + "/watch?session=" + sessionID + "&psk=" + psk)
	if err != nil {
		t.Fatalf("second watch: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(second.Body)
		t.Fatalf("expected 429 for watcher over the cap, got %d: %s", second.StatusCode, body)
	}
	_ = store
}
