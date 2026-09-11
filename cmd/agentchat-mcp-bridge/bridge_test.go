package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestPartitionMessages(t *testing.T) {
	msgs := []map[string]any{
		envelope("agent-b", "a1", "message", 1),
		envelope("agent-c", "a1", "message", 2),
		envelope("agent-b", "a1", "task_result", 3),
		envelope("agent-c", "", "broadcast", 4),
	}

	matched, rest := partitionMessages(msgs, "agent-b", "")
	if len(matched) != 2 || len(rest) != 2 {
		t.Fatalf("expected 2 matched / 2 rest, got %d / %d", len(matched), len(rest))
	}

	matched, rest = partitionMessages(msgs, "", "broadcast")
	if len(matched) != 1 || len(rest) != 3 {
		t.Fatalf("expected 1 broadcast matched, got %d matched / %d rest", len(matched), len(rest))
	}

	// Empty filters match everything.
	matched, rest = partitionMessages(msgs, "", "")
	if len(matched) != 4 || len(rest) != 0 {
		t.Fatalf("expected all matched with empty filters, got %d / %d", len(matched), len(rest))
	}
}

func TestHoldbackBuffer(t *testing.T) {
	var h holdbackBuffer
	if got := h.take(); len(got) != 0 {
		t.Fatalf("expected empty holdback, got %d", len(got))
	}

	h.add([]map[string]any{{"n": 1}, {"n": 2}})
	h.add([]map[string]any{{"n": 3}})
	got := h.take()
	if len(got) != 3 || got[0]["n"] != 1 {
		t.Fatalf("expected FIFO [1 2 3], got %v", got)
	}
	if got := h.take(); len(got) != 0 {
		t.Fatalf("expected holdback drained, got %d", len(got))
	}

	// Overflow drops the oldest.
	for i := 0; i < holdbackCap+5; i++ {
		h.add([]map[string]any{{"n": i}})
	}
	got = h.take()
	if len(got) != holdbackCap {
		t.Fatalf("expected holdback capped at %d, got %d", holdbackCap, len(got))
	}
	if got[0]["n"] != 5 {
		t.Fatalf("expected 5 oldest entries dropped, first entry is %v", got[0]["n"])
	}
}

// TestDrainAllMailboxOnly verifies drainAll reads only the mailbox — merging
// history re-delivered other agents' private traffic on every poll.
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

// TestDrainAllHoldbackRoundTrip: messages held back by a filtered wait must
// survive a failed poll and be returned by the next successful one.
func TestDrainAllHoldbackRoundTrip(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/s1/mailbox", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{envelope("agent-b", "a1", "message", 9)},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	b.holdback.add([]map[string]any{{"held": true}})

	if _, err := b.drainAll(); err == nil {
		t.Fatal("expected error while mailbox endpoint fails")
	}

	fail.Store(false)
	msgs, err := b.drainAll()
	if err != nil {
		t.Fatalf("drainAll: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected held-back + fresh message, got %d", len(msgs))
	}
	if msgs[0]["held"] != true {
		t.Fatalf("expected held-back message first, got %v", msgs[0])
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
