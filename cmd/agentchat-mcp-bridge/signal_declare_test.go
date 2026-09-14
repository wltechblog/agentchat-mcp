package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/mcp"
)

// jsonRPCLine feeds one request line through RunWith and returns the
// response matched by ID (requests are dispatched concurrently).
func jsonRPCLine(t *testing.T, s *mcp.Server, line string, id string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	s.RunWith(ctx, strings.NewReader(line+"\n"), &out)
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r struct {
			ID     json.RawMessage  `json:"id"`
			Result *json.RawMessage `json:"result"`
		}
		if json.Unmarshal([]byte(l), &r) != nil || r.Result == nil {
			continue
		}
		if string(r.ID) == `"`+id+`"` {
			var m map[string]any
			if err := json.Unmarshal(*r.Result, &m); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			return m
		}
	}
	t.Fatalf("no response with id %q in %q", id, out.String())
	return nil
}

// TestInitializeDeclaresSignals verifies the bridge's initialize result
// carries signals.actions — the self-declaration joist/gino auto-register.
func TestInitializeDeclaresSignals(t *testing.T) {
	s := mcp.NewServer("agentchat-mcp-bridge", "test")
	b := &Bridge{agentID: "a1"}
	registerSignals(s, b)

	result := jsonRPCLine(t, s, `{"jsonrpc":"2.0","id":"i1","method":"initialize","params":{}}`, "i1")
	signals, ok := result["signals"].(map[string]any)
	if !ok {
		t.Fatalf("expected signals block in initialize result, got %v", result)
	}
	actions, _ := signals["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("expected 1 declared action, got %d", len(actions))
	}
	sa := actions[0].(map[string]any)
	if sa["name"] != "check_messages" {
		t.Fatalf("expected check_messages, got %v", sa["name"])
	}
	if sa["response"] == "" {
		t.Fatal("declared action must carry a response template")
	}
}

// TestMetaOriginCaptured: a tools/call carrying _meta origin updates the
// origin used by wake-up signals, so the signal names the chat session that
// invoked the bridge.
func TestMetaOriginCaptured(t *testing.T) {
	// Mock agentchat server: /sessions/s1/agents returns a list.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"agent_id":"a1","online":true}]`))
	}))
	defer srv.Close()

	s := mcp.NewServer("agentchat-mcp-bridge", "test")
	b := &Bridge{httpBase: srv.URL, sessionID: "s1", psk: "p", agentID: "a1",
		client: srv.Client(), initialized: true}
	registerTools(s, b)

	line := `{"jsonrpc":"2.0","id":"t1","method":"tools/call","params":{"name":"list_agents","arguments":{},"_meta":{"channel":"telegram","chat_id":"8113382039"}}}`
	jsonRPCLine(t, s, line, "t1")

	ch, id := b.originTarget()
	if ch != "telegram" || id != "8113382039" {
		t.Fatalf("expected origin telegram:8113382039, got %s:%s", ch, id)
	}
}

// TestSignalSourceUsesMCPID: joist injects JOIST_MCP_ID (the config key);
// the signal source must match it or IsAllowed rejects the signal.
func TestSignalSourceUsesMCPID(t *testing.T) {
	b := &Bridge{mcpID: "team-chat"}
	if src := b.signalSource(); src != "team-chat" {
		t.Fatalf("expected team-chat, got %s", src)
	}
	b2 := &Bridge{}
	if src := b2.signalSource(); src != "agentchat" {
		t.Fatalf("expected fallback agentchat, got %s", src)
	}
}

// TestWakeUpSignalCarriesOrigin: end-to-end — _meta stamped tools/call,
// then a wake-up signal lands on a local socket with the session target
// and the host-injected source identity.
func TestWakeUpSignalCarriesOrigin(t *testing.T) {
	dir := t.TempDir()
	sock := dir + "/sig.sock"
	received := make(chan map[string]any, 1)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _ := conn.Read(buf)
		var sp map[string]any
		json.Unmarshal(buf[:n], &sp)
		received <- sp
		conn.Write([]byte(`{"status":"ok"}`))
	}()

	// Simulate the host agent calling a tool from discord thread-42.
	s := mcp.NewServer("t", "1")
	s.RegisterTool(mcp.Tool{
		Name:        "x",
		Description: "dummy",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (string, error) { return "ok", nil })
	line := `{"jsonrpc":"2.0","id":"t1","method":"tools/call","params":{"name":"x","arguments":{},"_meta":{"channel":"discord","chat_id":"thread-42"}}}`
	jsonRPCLine(t, s, line, "t1")

	b := &Bridge{agentID: "a1", mcpID: "wltb", signalSocketPath: sock}
	if err := b.sendCheckMessagesSignal(); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case sp := <-received:
		if sp["source"] != "wltb" {
			t.Fatalf("expected source wltb, got %v", sp["source"])
		}
		if sp["action"] != "check_messages" {
			t.Fatalf("expected check_messages, got %v", sp["action"])
		}
		if sp["channel"] != "discord" || sp["chat_id"] != "thread-42" {
			t.Fatalf("expected discord:thread-42, got %v:%v", sp["channel"], sp["chat_id"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no signal received on socket")
	}
}
