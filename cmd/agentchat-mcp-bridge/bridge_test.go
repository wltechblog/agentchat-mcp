package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testBridge(srv *httptest.Server) *Bridge {
	b := &Bridge{
		httpBase:  srv.URL,
		sessionID: "s1",
		psk:       "p",
		agentID:   "a1",
		client:    &http.Client{},
		sseClient: &http.Client{},
		signalCh:  make(chan struct{}, 1),
	}
	// Skip registration round-trips in doRequest.
	b.initialized = true
	return b
}

func envelope(from, to, msgType string, seq int64) map[string]any {
	return map[string]any{
		"id":       "m-1",
		"received": "t",
		"envelope": map[string]any{
			"type": msgType, "from": from, "to": to, "sequence": seq,
		},
	}
}

func TestTagDelivery(t *testing.T) {
	cases := []struct {
		to   string
		want string
	}{
		{"", "broadcast"},     // hub.Broadcast leaves to empty
		{"*", "broadcast"},    // API-level notify marks broadcasts with *
		{"agent-b", "direct"}, // SendMessage always sets a target
	}
	for _, c := range cases {
		m := map[string]any{"envelope": map[string]any{"to": c.to}}
		if got := tagDelivery(m); got != c.want {
			t.Errorf("tagDelivery(to=%q) = %q, want %q", c.to, got, c.want)
		}
	}
}

func TestDrainAllMailboxOnly(t *testing.T) {
	var historyHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/s1/mailbox", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{envelope("agent-b", "a1", "message", 7)},
		})
	})
	mux.HandleFunc("GET /sessions/s1/history", func(w http.ResponseWriter, r *http.Request) {
		historyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	msgs, err := b.drainAll()
	if err != nil {
		t.Fatalf("drainAll: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["_source"] != "mailbox" || msgs[0]["_delivery"] != "direct" {
		t.Fatalf("unexpected tags: %v / %v", msgs[0]["_source"], msgs[0]["_delivery"])
	}
	if hits := historyHits.Load(); hits != 0 {
		t.Fatalf("drainAll must not fetch history, got %d history calls", hits)
	}
}

func TestRequestSignalCoalesces(t *testing.T) {
	b := &Bridge{signalCh: make(chan struct{}, 1)}
	for i := 0; i < 100; i++ {
		b.requestSignal("test")
	}
	if len(b.signalCh) != 1 {
		t.Fatalf("expected requests to coalesce to 1 pending signal, got %d", len(b.signalCh))
	}
}

// TestSignalLoopRetriesUntilConnected: a signal send to a socket that isn't
// up yet must keep retrying and deliver once the listener appears — a missed
// wake-up is never final.
// maybeWakeOnConnect exercises the connect-time wake-up decision: peek the
// mailbox first; empty mailbox → no signal, pending mail → signal, peek
// failure → wake anyway (fail open).
func TestConnectWakeUpSkippedOnEmptyMailbox(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/s1/mailbox/peek", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"count": 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	fired, err := b.maybeWakeOnConnect()
	if err != nil {
		t.Fatalf("maybeWakeOnConnect: %v", err)
	}
	if fired {
		t.Fatal("expected NO wake-up on empty mailbox")
	}
}

func TestConnectWakeUpFiresWhenMailPending(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/s1/mailbox/peek", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"count": 2})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	fired, err := b.maybeWakeOnConnect()
	if err != nil {
		t.Fatalf("maybeWakeOnConnect: %v", err)
	}
	if !fired {
		t.Fatal("expected wake-up when 2 messages pending")
	}
}

func TestConnectWakeUpFailsOpenOnPeekError(t *testing.T) {
	// No peek route → 404 → error → must still wake.
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()

	b := testBridge(srv)
	fired, _ := b.maybeWakeOnConnect()
	if !fired {
		t.Fatal("peek failure must fail open (wake anyway)")
	}
}

func TestSignalLoopRetriesUntilConnected(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "sig.sock")

	var got atomic.Int32
	accept := func(ln net.Listener) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			got.Add(1)
			conn.Write([]byte(`{"status":"ok"}`))
			conn.Close()
		}
	}

	b := &Bridge{signalSocketPath: sock, signalCh: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		b.signalLoop(ctx)
		close(done)
	}()

	b.requestSignal("test")
	time.Sleep(800 * time.Millisecond) // let the first sends fail against the missing socket

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go accept(ln)

	deadline := time.Now().Add(5 * time.Second)
	for got.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("signal loop never delivered after the socket came up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signalLoop did not exit after context cancel")
	}
}

func TestBackoffJitterStaysBounded(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := backoffJitter(500*time.Millisecond, 30*time.Second)
		if d < 250*time.Millisecond || d > 500*time.Millisecond {
			t.Fatalf("jitter out of [d/2, d] range: %v", d)
		}
	}
	if got := backoffJitter(time.Hour, 30*time.Second); got > 30*time.Second {
		t.Fatalf("jitter not capped at max: %v", got)
	}
}

// TestHandleSSEEventSignalsOnSystemEvents: with the unified pipeline the
// stream also carries scratchpad and leader events — the bridge should wake
// the agent for those (unless it made the change itself).
func TestHandleSSEEventSignalsOnSystemEvents(t *testing.T) {
	lastSeq.Store(0) // shared global; isolate this test
	b := &Bridge{agentID: "a1", signalCh: make(chan struct{}, 1)}

	envJSON := func(msgType, from, to string, seq int64) string {
		data, _ := json.Marshal(map[string]any{"type": msgType, "from": from, "to": to, "sequence": seq})
		return string(data)
	}

	expectSignal := func(msg string) {
		t.Helper()
		if len(b.signalCh) != 1 {
			t.Fatalf("%s: expected a pending signal", msg)
		}
		<-b.signalCh
	}
	expectQuiet := func(msg string) {
		t.Helper()
		if len(b.signalCh) != 0 {
			t.Fatalf("%s: expected no signal", msg)
		}
	}

	b.handleSSEEvent("message", envJSON("scratchpad_update", "agent-b", "*", 1))
	expectSignal("scratchpad_update from another agent")

	b.handleSSEEvent("message", envJSON("scratchpad_update", "a1", "*", 2))
	expectQuiet("own scratchpad_update")

	b.handleSSEEvent("message", envJSON("leader_info", "server", "*", 3))
	expectSignal("leader_info")

	b.handleSSEEvent("message", envJSON("agent_joined", "server", "*", 4))
	expectQuiet("agent_joined")

	b.handleSSEEvent("message", envJSON("message", "agent-b", "a1", 5))
	expectSignal("direct message")
}

// TestEnsureInitCapturesWatchToken: register issues a short-lived watch
// token so /watch URLs never carry the PSK; the bridge must store it and be
// able to drop it when the server rejects it.
func TestEnsureInitCapturesWatchToken(t *testing.T) {
	var registerHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/s1/register", func(w http.ResponseWriter, r *http.Request) {
		registerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":      "registered",
			"watch_token": fmt.Sprintf("tok-%d", registerHits.Load()),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	b.initialized = false

	if err := b.ensureInit(); err != nil {
		t.Fatalf("ensureInit: %v", err)
	}
	if got := b.getWatchToken(); got != "tok-1" {
		t.Fatalf("expected watch token tok-1, got %q", got)
	}

	// A server-side rejection must drop registration state so the next call
	// re-registers with fresh credentials.
	b.invalidateRegistration()
	if got := b.getWatchToken(); got != "" {
		t.Fatalf("expected watch token cleared on invalidate, got %q", got)
	}
	if err := b.ensureInit(); err != nil {
		t.Fatalf("ensureInit after invalidate: %v", err)
	}
	if got := b.getWatchToken(); got != "tok-2" {
		t.Fatalf("expected fresh token tok-2, got %q", got)
	}
}

// TestPollMailboxFiltered: filtered drains hit the long-poll endpoint with
// the filters encoded as query params.
func TestPollMailboxFiltered(t *testing.T) {
	var gotQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/s1/mailbox", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{envelope("agent-b", "a1", "task_result", 3)},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	msgs, err := b.pollMailbox(25*time.Second, "agent-b", "task_result")
	if err != nil {
		t.Fatalf("pollMailbox: %v", err)
	}

	if gotQuery.Get("from") != "agent-b" || gotQuery.Get("type") != "task_result" {
		t.Fatalf("filters not forwarded: %v", gotQuery)
	}
	if gotQuery.Get("wait") != "25" {
		t.Fatalf("expected wait=25 forwarded, got %q", gotQuery.Get("wait"))
	}
	if len(msgs) != 1 || msgs[0]["_delivery"] != "direct" {
		t.Fatalf("unexpected messages: %v", msgs)
	}
}

// TestUnauthorizedInvalidates: when the server rejects our credentials
// (401/403), registration state must drop so the next call re-registers.
func TestUnauthorizedInvalidates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/s1/mailbox", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	b.watchToken = "tok"

	if _, err := b.doJSON("GET", "/sessions/s1/mailbox", nil); err == nil {
		t.Fatal("expected error from 401 response")
	}
	if b.getWatchToken() != "" {
		t.Fatalf("watch token should be cleared on 401, got %q", b.getWatchToken())
	}
	if b.initialized {
		t.Fatal("initialized should be false after 401")
	}
}

// TestDrainAllStampsSignalTrigger: the message whose sequence triggered the
// most recent wake-up signal is stamped so the agent can see why it woke.
func TestDrainAllStampsSignalTrigger(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/s1/mailbox", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				envelope("agent-b", "a1", "message", 7),
				envelope("agent-c", "a1", "message", 8),
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	pendingSignal.Store(&pendingSignalInfo{TriggerType: "message", FromAgent: "agent-b", Sequence: 7})

	msgs, err := b.drainAll()
	if err != nil {
		t.Fatalf("drainAll: %v", err)
	}
	if stamped, ok := msgs[0]["_signal_trigger"].(bool); !ok || !stamped {
		t.Fatalf("expected seq-7 message stamped as signal trigger, got %v", msgs[0])
	}
	if _, ok := msgs[1]["_signal_trigger"]; ok {
		t.Fatalf("seq-8 message must not be stamped")
	}
	if getPendingSignalInfo() != nil {
		t.Fatal("pending signal info should be consumed by the drain")
	}
}
