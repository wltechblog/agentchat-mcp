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
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}
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
	result := resp.Result.(map[string]any)
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
	result := resp.Result.(map[string]any)
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

	var toolsResp jsonRPCResponse
	json.Unmarshal([]byte(lines[1]), &toolsResp)
	result := toolsResp.Result.(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "ping" {
		t.Fatalf("expected ping tool, got %v", tools)
	}
}
