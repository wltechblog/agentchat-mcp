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
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
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

type ToolHandler func(args map[string]any) (string, error)

type Server struct {
	name     string
	version  string
	tools    []Tool
	handlers map[string]ToolHandler
	writer   *bufio.Writer
	mu       sync.Mutex
}

func NewServer(name, version string) *Server {
	return &Server{
		name:     name,
		version:  version,
		tools:    []Tool{},
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

func (s *Server) RunWith(ctx context.Context, in io.Reader, out io.Writer) error {
	s.writer = bufio.NewWriter(out)

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

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
			return io.EOF
		}

		line := scanner.Bytes()
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

		s.handleRequest(req)
	}
}

func (s *Server) handleRequest(req jsonRPCRequest) {
	switch req.Method {
	case "initialize":
		s.sendResult(req.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]string{
				"name":    s.name,
				"version": s.version,
			},
		})

	case "notifications/initialized":
		// no-op

	case "tools/list":
		s.sendResult(req.ID, map[string]any{
			"tools": s.tools,
		})

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.sendError(req.ID, -32602, "invalid params")
			return
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
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
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
