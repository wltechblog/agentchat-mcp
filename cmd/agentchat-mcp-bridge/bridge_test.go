package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"

	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/mcp"
)

func testBridge(srv *httptest.Server) *Bridge {
	b := &Bridge{
		sessionID:  "s1",
		psk:        "p",
		agentID:    "a1",
		client:     &http.Client{},
		sseClient:  &http.Client{},
		fileClient: &http.Client{},
		signalCh:   make(chan struct{}, 1),
	}
	// Skip registration round-trips in doRequest.
	b.initialized = true
	if srv != nil {
		b.httpBase = srv.URL
	}
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
// stream also carries scratchpad events — the bridge should wake
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

// TestRequireString: the diagnostic error must state exactly what the tool
// call received — bare "to is required" was unfalsifiable after the fact
// (joinery bug report: send_message "to is required" flake).
func TestRequireString(t *testing.T) {
	v, err := requireString(map[string]any{"to": "agent-b"}, "to", "send_message")
	if err != nil || v != "agent-b" {
		t.Fatalf("valid arg rejected: %q %v", v, err)
	}

	cases := []struct {
		name        string
		args        map[string]any
		wantInError string
	}{
		{"missing", map[string]any{"payload": map[string]any{"text": "hi"}}, "to=absent"},
		{"empty", map[string]any{"to": "", "payload": "p"}, `to=""`},
		{"non-string", map[string]any{"to": 42.0}, "to=42"},
		{"null", map[string]any{"to": nil}, "to=absent"},
	}
	for _, tc := range cases {
		_, err := requireString(tc.args, "to", "send_message")
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		msg := err.Error()
		for _, want := range []string{"send_message", "to=", tc.wantInError} {
			if !strings.Contains(msg, want) {
				t.Fatalf("%s: error %q missing %q", tc.name, msg, want)
			}
		}
	}

	// Received keys are listed so a misnested argument (e.g. `to` at the top
	// level of the call instead of inside arguments) is visible in the error.
	_, err = requireString(map[string]any{"payload": "p"}, "to", "send_message")
	if !strings.Contains(err.Error(), "payload") {
		t.Fatalf("expected received arg keys in error, got %v", err)
	}

	// Misnested recovery: the exact field-report shape — to inside payload,
	// absent at the top level — is adopted, stripped from the payload, and
	// the send proceeds.
	payload := map[string]any{"message": "hello", "to": "hypecast-core"}
	v, err = requireString(map[string]any{"payload": payload}, "to", "send_message")
	if err != nil || v != "hypecast-core" {
		t.Fatalf("expected misnested to recovered as hypecast-core, got %q %v", v, err)
	}
	if _, still := payload["to"]; still {
		t.Fatal("recovered to must be stripped from the payload")
	}
	if _, has := payload["message"]; !has {
		t.Fatal("payload content must survive recovery")
	}

	// A non-string payload value is not recovery material.
	if _, err := requireString(map[string]any{"payload": map[string]any{"to": 42.0}}, "to", "send_message"); err == nil {
		t.Fatal("non-string misnested value must not be recovered")
	}
}

// runToolCall drives a single tools/call through the MCP dispatcher over
// in-memory pipes, exactly as a host would, and returns the tool's result
// text — or an error carrying the JSON-RPC error message for tool failures.
func runToolCall(t *testing.T, b *Bridge, name string, args map[string]any) (string, error) {
	t.Helper()
	s := mcp.NewServer("test", "0.0")
	registerTools(s, b)
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	var out bytes.Buffer
	if err := s.RunWith(context.Background(), bytes.NewReader(append(req, '\n')), &out); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	var resp struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("%s", resp.Error.Message)
	}
	if resp.Result == nil || len(resp.Result.Content) == 0 {
		t.Fatalf("no content in response: %s", out.String())
	}
	return resp.Result.Content[0].Text, nil
}

// TestSendFileFromPath: the file's bytes are streamed verbatim from disk to
// the upload endpoint — never through the tool arguments.
func TestSendFileFromPath(t *testing.T) {
	content := bytes.Repeat([]byte("agentchat file transfer "), 4000)
	srcPath := filepath.Join(t.TempDir(), "report.bin")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)

	var gotUpload []byte
	var gotFilename string
	var shares int
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/s1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		gotFilename = r.URL.Query().Get("filename")
		gotUpload, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"file_id":"f1","size":%d,"sha256":%q}`, len(gotUpload), hex.EncodeToString(sum[:]))
	})
	mux.HandleFunc("/sessions/s1/messages", func(w http.ResponseWriter, r *http.Request) {
		shares++
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{}")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := testBridge(srv)
	result, err := runToolCall(t, b, "send_file", map[string]any{"to": "agent-b", "path": srcPath})
	if err != nil {
		t.Fatalf("send_file: %v", err)
	}
	if !bytes.Equal(gotUpload, content) {
		t.Fatalf("uploaded bytes differ from the source file (%d vs %d)", len(gotUpload), len(content))
	}
	if gotFilename != "report.bin" {
		t.Fatalf("filename should default to the base name, got %q", gotFilename)
	}
	if shares != 1 {
		t.Fatalf("expected exactly one file_share message, got %d", shares)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(result), &sent); err != nil {
		t.Fatalf("decode result %q: %v", result, err)
	}
	if sent["file_id"] != "f1" || sent["size"] != float64(len(content)) || sent["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("unexpected result: %v", sent)
	}
}

// TestSendFileIntegrityMismatchFails: a server-stored size/sha that differs
// from the streamed file must fail the send and skip the share message.
func TestSendFileIntegrityMismatchFails(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "data.bin")
	if err := os.WriteFile(srcPath, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	var shares int
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/s1/files", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"file_id":"f1","size":999,"sha256":"deadbeef"}`)
	})
	mux.HandleFunc("/sessions/s1/messages", func(w http.ResponseWriter, r *http.Request) {
		shares++
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{}")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := runToolCall(t, testBridge(srv), "send_file", map[string]any{"to": "agent-b", "path": srcPath})
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected integrity failure, got %v", err)
	}
	if shares != 0 {
		t.Fatalf("corrupt upload must not be shared, got %d share messages", shares)
	}
}

// TestSendFileMissingPath: content_base64 is gone — a local path is the only
// way in, and a missing or unreadable one is a clear error.
func TestSendFileMissingPath(t *testing.T) {
	_, err := runToolCall(t, testBridge(nil), "send_file", map[string]any{"to": "agent-b"})
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("expected path-required error, got %v", err)
	}
	_, err = runToolCall(t, testBridge(nil), "send_file", map[string]any{"to": "agent-b", "path": "/nonexistent/file.bin"})
	if err == nil || !strings.Contains(err.Error(), "cannot open") {
		t.Fatalf("expected open error for bad path, got %v", err)
	}
}

// TestDownloadFileToPath: bytes stream to the destination file, integrity is
// verified end-to-end against expected_sha256.
func TestDownloadFileToPath(t *testing.T) {
	content := bytes.Repeat([]byte("\x00\x01\xfe\xffbinary!"), 3000)
	sum := sha256.Sum256(content)
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/s1/files/f9", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "received.bin")
	result, err := runToolCall(t, testBridge(srv), "download_file", map[string]any{
		"file_id":         "f9",
		"path":            dest,
		"expected_sha256": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("download_file: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("destination file not written: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("saved file corrupted (%d vs %d bytes)", len(got), len(content))
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(result), &saved); err != nil {
		t.Fatalf("decode result %q: %v", result, err)
	}
	if saved["path"] != dest || saved["size"] != float64(len(content)) {
		t.Fatalf("unexpected result: %v", saved)
	}
}

// TestDownloadShaMismatchSavesNothing: an end-to-end sha mismatch must fail
// the download and leave nothing at the destination path.
func TestDownloadShaMismatchSavesNothing(t *testing.T) {
	content := []byte("actual bytes")
	sum := sha256.Sum256(content)
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/s1/files/f9", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "received.bin")
	_, err := runToolCall(t, testBridge(srv), "download_file", map[string]any{
		"file_id":         "f9",
		"path":            dest,
		"expected_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected sha mismatch error, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("failed download must not leave a file at the destination")
	}
}

// TestDownloadRequiresPath: files are saved to disk — the base64-return
// mode is gone, so a missing path is an error, not a content dump.
func TestDownloadRequiresPath(t *testing.T) {
	_, err := runToolCall(t, testBridge(nil), "download_file", map[string]any{"file_id": "f9"})
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("expected path-required error, got %v", err)
	}
}
