package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInitialize(t *testing.T) {
	s := NewServer("test", "1.0.0")
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	req := jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"init-1"`), Method: "initialize", Params: json.RawMessage(`{}`)}
	s.handleRequest(req)

	var resp jsonRPCResponse
	json.Unmarshal(buf.Bytes(), &resp)
	if resp.JSONRPC != "2.0" {
		t.Fatalf("expected jsonrpc 2.0, got %s", resp.JSONRPC)
	}
	result := resultMap(t, resp)
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("expected protocol version %s, got %v", ProtocolVersion, result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "test" {
		t.Fatalf("expected server name 'test', got %v", info["name"])
	}
}

func TestToolsList(t *testing.T) {
	s := NewServer("test", "1.0.0")
	s.RegisterTool(Tool{
		Name:        "my_tool",
		Description: "does a thing",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (string, error) {
		return "ok", nil
	})
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	req := jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"list-1"`), Method: "tools/list"}
	s.handleRequest(req)

	var resp jsonRPCResponse
	json.Unmarshal(buf.Bytes(), &resp)
	result := resultMap(t, resp)
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "my_tool" {
		t.Fatalf("expected my_tool, got %v", tool["name"])
	}
}

func TestToolCall(t *testing.T) {
	s := NewServer("test", "1.0.0")
	s.RegisterTool(Tool{
		Name:        "echo",
		Description: "echoes input",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (string, error) {
		msg, _ := args["msg"].(string)
		return msg, nil
	})
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	params, _ := json.Marshal(map[string]any{
		"name":      "echo",
		"arguments": map[string]string{"msg": "hello"},
	})
	req := jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"call-1"`), Method: "tools/call", Params: params}
	s.handleRequest(req)

	var resp jsonRPCResponse
	json.Unmarshal(buf.Bytes(), &resp)
	result := resultMap(t, resp)
	content := result["content"].([]any)
	text := content[0].(map[string]any)
	if text["text"] != "hello" {
		t.Fatalf("expected 'hello', got %v", text["text"])
	}
}

func TestToolCallError(t *testing.T) {
	s := NewServer("test", "1.0.0")
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	params, _ := json.Marshal(map[string]any{
		"name":      "nonexistent",
		"arguments": map[string]any{},
	})
	req := jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"call-2"`), Method: "tools/call", Params: params}
	s.handleRequest(req)

	var resp jsonRPCResponse
	json.Unmarshal(buf.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32601 {
		t.Fatalf("expected error code -32601, got %d", resp.Error.Code)
	}
}

func TestNotificationIgnored(t *testing.T) {
	s := NewServer("test", "1.0.0")
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	req := jsonRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	s.handleRequest(req)

	if buf.Len() > 0 {
		t.Fatalf("expected no response for notification, got: %s", buf.String())
	}
}

func TestUnknownMethod(t *testing.T) {
	s := NewServer("test", "1.0.0")
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	req := jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"unknown-1"`), Method: "foo/bar"}
	s.handleRequest(req)

	var resp jsonRPCResponse
	json.Unmarshal(buf.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("expected -32601 error, got %v", resp.Error)
	}
}

func TestRunWithIO(t *testing.T) {
	s := NewServer("test", "1.0.0")
	s.RegisterTool(Tool{
		Name:        "ping",
		Description: "pong",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (string, error) {
		return "pong", nil
	})

	initReq, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"i1"`), Method: "initialize", Params: json.RawMessage(`{}`)})
	toolsReq, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"i2"`), Method: "tools/list"})

	input := strings.NewReader(string(initReq) + "\n" + string(toolsReq) + "\n")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.RunWith(ctx, input, &out)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 responses, got %d: %s", len(lines), out.String())
	}

	var initResp jsonRPCResponse
	json.Unmarshal([]byte(lines[0]), &initResp)
	if initResp.Result == nil {
		t.Fatal("expected init result")
	}

	// Requests are dispatched concurrently, so match responses by id rather
	// than position.
	var toolsResp jsonRPCResponse
	for _, line := range lines {
		var r jsonRPCResponse
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if string(r.ID) == `"i2"` {
			toolsResp = r
		}
	}
	result := resultMap(t, toolsResp)
	tools := result["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "ping" {
		t.Fatalf("expected ping tool, got %v", tools)
	}
}

// resultMap decodes a response's result object.
func resultMap(t *testing.T, resp jsonRPCResponse) map[string]any {
	t.Helper()
	if resp.Result == nil {
		t.Fatal("expected result")
	}
	var m map[string]any
	if err := json.Unmarshal(*resp.Result, &m); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return m
}

func TestPing(t *testing.T) {
	s := NewServer("test", "1.0.0")
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	req := jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"p1"`), Method: "ping"}
	s.handleRequest(req)

	var resp jsonRPCResponse
	json.Unmarshal(buf.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("ping must not error, got %v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("ping must return a result")
	}
}

// TestEmptyStringResultEmitted: a tool returning "" must produce
// "result":"" — omitempty on a plain any dropped it entirely, yielding a
// response with neither result nor error.
func TestEmptyStringResultEmitted(t *testing.T) {
	s := NewServer("test", "1.0.0")
	s.RegisterTool(Tool{
		Name:        "empty",
		Description: "returns empty string",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (string, error) {
		return "", nil
	})
	var buf bytes.Buffer
	s.writer = bufio.NewWriter(&buf)

	params, _ := json.Marshal(map[string]any{"name": "empty", "arguments": map[string]any{}})
	s.handleRequest(jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"e1"`), Method: "tools/call", Params: params})

	out := buf.String()
	if !strings.Contains(out, `"result"`) {
		t.Fatalf("expected result in response, got %s", out)
	}
	if !strings.Contains(out, `"text":""`) {
		t.Fatalf("expected empty text to survive marshaling, got %s", out)
	}
}

// TestConcurrentDispatch: a blocking tool call must not delay subsequent
// requests — two slow calls should overlap, not serialize.
func TestConcurrentDispatch(t *testing.T) {
	s := NewServer("test", "1.0.0")
	s.RegisterTool(Tool{
		Name:        "slow",
		Description: "takes a while",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (string, error) {
		time.Sleep(300 * time.Millisecond)
		return "done", nil
	})

	call := func(id string) []byte {
		b, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Method: "tools/call",
			Params: json.RawMessage(`{"name":"slow","arguments":{}}`)})
		return b
	}
	input := strings.NewReader(string(call("s1")) + "\n" + string(call("s2")) + "\n")
	var out bytes.Buffer

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.RunWith(ctx, input, &out)
	elapsed := time.Since(start)

	if elapsed >= 600*time.Millisecond {
		t.Fatalf("slow calls serialized (%v); requests must dispatch concurrently", elapsed)
	}

	var found int
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r jsonRPCResponse
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if string(r.ID) == `"s1"` || string(r.ID) == `"s2"` {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expected both responses, got %d in: %s", found, out.String())
	}
}
