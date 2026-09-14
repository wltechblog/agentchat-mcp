package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

const ProtocolVersion = "2024-11-05"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	// Result is a pointer so a legitimately empty result (e.g. "") still
	// marshals as "result":"" — omitempty on a plain any would drop it and
	// produce a response with neither result nor error.
	Result *json.RawMessage `json:"result,omitempty"`
	Error  *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// SignalAction describes a signal action this server can fire at its host
// agent (joist / gino / picobot signal systems). Self-declared via the
// initialize result so hosts auto-register the actions without config.
// Wire format matches joist's mcp.SignalAction contract.
type SignalAction struct {
	// Name is the signal action name (e.g. "check_messages").
	Name string `json:"name"`

	// Description is a human-readable description of what the signal means.
	Description string `json:"description,omitempty"`

	// Response is the safe response template injected into the host agent
	// when the signal fires. Supports {{.Source}}, {{.Action}},
	// {{.Timestamp}}, {{.Time}}, {{.Channel}}, {{.ChatID}}.
	Response string `json:"response,omitempty"`

	// Silent suppresses the host's channel reply (agent still processes).
	Silent bool `json:"silent,omitempty"`
}

type ToolHandler func(args map[string]any) (string, error)

type Server struct {
	name     string
	version  string
	tools    []Tool
	handlers map[string]ToolHandler
	// signals are self-declared signal actions surfaced in the initialize
	// result so hosts (joist / gino) auto-register them.
	signals []SignalAction
	writer  *bufio.Writer
	mu      sync.Mutex
}

func NewServer(name, version string) *Server {
	return &Server{
		name:     name,
		version:  version,
		tools:    []Tool{},
		signals:  []SignalAction{},
		handlers: make(map[string]ToolHandler),
		writer:   bufio.NewWriter(os.Stdout),
	}
}

func (s *Server) RegisterTool(tool Tool, handler ToolHandler) {
	s.tools = append(s.tools, tool)
	s.handlers[tool.Name] = handler
}

func (s *Server) Run(ctx context.Context) error {
	return s.RunWith(ctx, os.Stdin, os.Stdout)
}

// shutdownGrace bounds how long RunWith waits for in-flight handlers after
// the input stream ends. The host will not read further responses, so an
// unbounded wait (handlers may long-poll for minutes) would hang shutdown.
const shutdownGrace = 2 * time.Second

func (s *Server) RunWith(ctx context.Context, in io.Reader, out io.Writer) error {
	s.writer = bufio.NewWriter(out)

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var inFlight sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !scanner.Scan() {
			if scanner.Err() != nil {
				return scanner.Err()
			}
			break
		}

		line := append([]byte(nil), scanner.Bytes()...) // scanner reuses its buffer; handlers run async
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Debug("skipping non-JSON line", "error", err)
			continue
		}

		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}

		// Dispatch each request in its own goroutine: a tool can block for
		// minutes (wait_for_message long-polls server-side), and the host
		// must still be able to call other tools and get ping responses.
		// Responses are serialized by s.mu; JSON-RPC ids let the host match
		// responses that arrive out of order.
		inFlight.Add(1)
		go func(req jsonRPCRequest) {
			defer inFlight.Done()
			s.handleRequest(req)
		}(req)
	}

	// Don't return while handlers are still writing responses.
	done := make(chan struct{})
	go func() {
		inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
	return nil
}

// RegisterSignal declares a signal action this server fires at its host.
// Declarations ride the initialize result (signals.actions) where joist /
// gino auto-register them — no config needed on the host side.
func (s *Server) RegisterSignal(sa SignalAction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = append(s.signals, sa)
}

// lastOrigin captures the most recent tools/call _meta origin
// (channel/chat_id stamped by the host agent). It is how the bridge learns
// which chat session invoked it, so wake-up signals can route back to that
// exact session.
var lastOrigin struct {
	sync.Mutex
	Channel string
	ChatID  string
}

// Origin returns the most recent _meta origin seen on a tools/call.
func Origin() (channel, chatID string) {
	lastOrigin.Lock()
	defer lastOrigin.Unlock()
	return lastOrigin.Channel, lastOrigin.ChatID
}

func (s *Server) handleRequest(req jsonRPCRequest) {
	switch req.Method {
	case "initialize":
		result := map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]string{
				"name":    s.name,
				"version": s.version,
			},
		}
		s.mu.Lock()
		if len(s.signals) > 0 {
			result["signals"] = map[string]any{
				"actions": s.signals,
			}
		}
		s.mu.Unlock()
		s.sendResult(req.ID, result)

	case "notifications/initialized":
		// no-op

	case "ping":
		s.sendResult(req.ID, map[string]any{})

	case "tools/list":
		s.sendResult(req.ID, map[string]any{
			"tools": s.tools,
		})

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      *struct {
				Channel string `json:"channel"`
				ChatID  string `json:"chat_id"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.sendError(req.ID, -32602, "invalid params")
			return
		}

		// Capture the calling session's origin so wake-up signals route to
		// the chat that invoked us (multi-session agents).
		if params.Meta != nil {
			lastOrigin.Lock()
			lastOrigin.Channel = params.Meta.Channel
			lastOrigin.ChatID = params.Meta.ChatID
			lastOrigin.Unlock()
		}

		handler, ok := s.handlers[params.Name]
		if !ok {
			s.sendError(req.ID, -32601, fmt.Sprintf("unknown tool: %s", params.Name))
			return
		}

		result, err := handler(params.Arguments)
		if err != nil {
			s.sendError(req.ID, -32000, err.Error())
			return
		}

		s.sendResult(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": result},
			},
		})

	default:
		s.sendError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) sendResult(id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		// A result that can't marshal is as useless as an error response;
		// surface it as one so the host doesn't hang waiting.
		s.sendError(id, -32603, fmt.Sprintf("marshal result: %v", err))
		return
	}
	rm := json.RawMessage(raw)
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: &rm}
	s.write(resp)
}

func (s *Server) sendError(id json.RawMessage, code int, message string) {
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
	s.write(resp)
}

func (s *Server) write(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(v)
	s.writer.Write(data)
	s.writer.WriteByte('\n')
	s.writer.Flush()
}
