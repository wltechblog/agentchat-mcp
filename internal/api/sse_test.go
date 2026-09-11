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
	h := hub.New(store, leader.NewTracker(), scratchpad.NewStore(),
		filestore.NewStore(1<<20), pt, mailbox.NewStore(1000))
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
