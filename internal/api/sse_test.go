package api

import (
	"bufio"
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

// TestSSEKeepaliveAndRetry connects to /watch and verifies the stream starts
// with a reconnect hint and emits keepalive comment pings while idle. Without
// pings, proxies silently reap idle SSE streams and clients never notice.
func TestSSEKeepaliveAndRetry(t *testing.T) {
	store := session.NewStore()
	pt := presence.NewTracker(60 * time.Second)
	defer pt.Stop()
	h := hub.New(hub.Deps{SessionStore: store, Leader: leader.NewTracker(), Scratchpad: scratchpad.NewStore(),
		Files: filestore.NewStore(1 << 20), Presence: pt, Mailboxes: mailbox.NewStore(1000)})
	handler := New(h, store)
	handler.ssePingInterval = 20 * time.Millisecond // speed up for the test

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	sessionID, psk := createTestSession(t, server)

	resp, err := http.Get(server.URL + "/watch?session=" + sessionID + "&psk=" + psk)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	lines := make(chan string, 64)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	sawRetry, sawConnected, sawPing := false, false, false
	deadline := time.After(3 * time.Second)
	for !(sawRetry && sawConnected && sawPing) {
		select {
		case line := <-lines:
			switch {
			case strings.HasPrefix(line, "retry:"):
				sawRetry = true
			case strings.HasPrefix(line, "event: connected"):
				sawConnected = true
			case strings.HasPrefix(line, ": ping"):
				sawPing = true
			}
		case <-deadline:
			t.Fatalf("SSE stream incomplete: retry=%v connected=%v ping=%v", sawRetry, sawConnected, sawPing)
		}
	}
}

// TestSSELiveDelivery verifies messages sent after a client connects arrive
// as live events. Combined with subscribe-before-history in the handler,
// there is no window between connect and subscription where events are lost.
func TestSSELiveDelivery(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	resp, err := http.Get(server.URL + "/watch?session=" + sessionID + "&psk=" + psk)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 256)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	waitForLine(t, lines, "event: connected")

	// A broadcast sent after the client connected must arrive as a live event.
	broadcast := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/broadcast", sessionID, psk, "agent-1", map[string]any{
		"type":    "broadcast",
		"payload": map[string]string{"text": "live one"},
	})
	broadcast.Body.Close()

	waitForLine(t, lines, "event: message")
	waitForLine(t, lines, `data: {"type":"broadcast","session_id":"`+sessionID+`","from":"agent-1"`)
}

// waitForLine reads lines until one has the given prefix, failing on timeout.
func waitForLine(t *testing.T, lines <-chan string, prefix string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, prefix) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for line %q", prefix)
		}
	}
}

// TestWatchCarriesSystemEvents: after the pipeline unification, the watch
// stream carries scratchpad updates, leader changes, and joins — not just
// messages and broadcasts.
func TestWatchCarriesSystemEvents(t *testing.T) {
	server, _ := setupTestServer(t)
	sessionID, psk := createTestSession(t, server)
	registerAgent(t, server.URL, sessionID, psk, "agent-1", nil)

	resp, err := http.Get(server.URL + "/watch?session=" + sessionID + "&psk=" + psk)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 256)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	waitForLine(t, lines, "event: connected")

	set := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/scratchpad/set", sessionID, psk, "agent-1", map[string]any{
		"key": "plan", "value": "step 1",
	})
	set.Body.Close()
	waitForDataLine(t, lines, `{"type":"scratchpad_update"`)

	registerAgent(t, server.URL, sessionID, psk, "agent-2", nil)
	waitForDataLine(t, lines, `{"type":"agent_joined"`)

	xfer := doAuthRequest(t, server.URL, "POST", "/sessions/"+sessionID+"/leader/transfer", sessionID, psk, "agent-1", map[string]any{
		"new_leader_id": "agent-2",
	})
	xfer.Body.Close()
	waitForDataLine(t, lines, `{"type":"leader_info"`)
}

// waitForDataLine reads lines until a data line's JSON payload starts with
// the given prefix, failing on timeout.
func waitForDataLine(t *testing.T, lines <-chan string, payloadPrefix string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "data:") && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")), payloadPrefix) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for data payload with prefix %q", payloadPrefix)
		}
	}
}
