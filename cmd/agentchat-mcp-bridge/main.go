package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wltechblog/agentchat-mcp/internal/mcp"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

type Bridge struct {
	agentID   string
	sessionID string
	conn      *websocket.Conn
	writeMu   sync.Mutex

	pending   map[string]chan protocol.Envelope
	pendingMu sync.Mutex

	incoming []protocol.Envelope
	incMu    sync.Mutex
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	wsURL := requireEnv("AGENTCHAT_URL")
	sessionID := requireEnv("AGENTCHAT_SESSION_ID")
	psk := requireEnv("AGENTCHAT_PSK")
	agentID := requireEnv("AGENTCHAT_AGENT_ID")
	agentName := envOrDefault("AGENTCHAT_AGENT_NAME", agentID)
	capsStr := envOrDefault("AGENTCHAT_CAPABILITIES", "")
	var caps []string
	if capsStr != "" {
		caps = strings.Split(capsStr, ",")
		for i := range caps {
			caps[i] = strings.TrimSpace(caps[i])
		}
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		slog.Error("websocket connect failed", "error", err)
		os.Exit(1)
	}

	authPayload, _ := json.Marshal(protocol.AuthPayload{
		SessionID:    sessionID,
		AgentID:      agentID,
		AgentName:    agentName,
		PSK:          psk,
		Capabilities: caps,
	})
	authMsg, _ := json.Marshal(protocol.Envelope{
		Type:      protocol.TypeAuth,
		Payload:   authPayload,
		Timestamp: time.Now().UTC(),
	})
	if err := conn.WriteMessage(websocket.TextMessage, authMsg); err != nil {
		slog.Error("auth write failed", "error", err)
		os.Exit(1)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, resp, err := conn.ReadMessage()
	if err != nil {
		slog.Error("auth read failed", "error", err)
		os.Exit(1)
	}
	var authResp protocol.Envelope
	json.Unmarshal(resp, &authResp)
	if authResp.Type != protocol.TypeAuthOK {
		slog.Error("auth rejected", "response", string(resp))
		os.Exit(1)
	}
	conn.SetReadDeadline(time.Time{})

	bridge := &Bridge{
		agentID:   agentID,
		sessionID: sessionID,
		conn:      conn,
		pending:   make(map[string]chan protocol.Envelope),
	}

	go bridge.readPump()

	server := mcp.NewServer("agentchat-mcp-bridge", "1.0.0")
	registerTools(server, bridge)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slog.Info("bridge ready", "agent_id", agentID, "session_id", sessionID)
	if err := server.Run(ctx); err != nil {
		slog.Error("MCP server exited", "error", err)
	}
}

func (b *Bridge) sendWS(env protocol.Envelope) error {
	env.SessionID = b.sessionID
	env.From = b.agentID
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.conn.WriteMessage(websocket.TextMessage, data)
}

func (b *Bridge) sendAndWait(env protocol.Envelope, responseType string, timeout time.Duration) (protocol.Envelope, error) {
	ch := make(chan protocol.Envelope, 1)
	b.pendingMu.Lock()
	b.pending[responseType] = ch
	b.pendingMu.Unlock()
	defer func() {
		b.pendingMu.Lock()
		delete(b.pending, responseType)
		b.pendingMu.Unlock()
	}()

	if err := b.sendWS(env); err != nil {
		return protocol.Envelope{}, err
	}

	select {
	case resp := <-ch:
		if resp.Type == protocol.TypeError {
			var errData map[string]string
			json.Unmarshal(resp.Payload, &errData)
			return protocol.Envelope{}, fmt.Errorf("%s", errData["error"])
		}
		return resp, nil
	case <-time.After(timeout):
		return protocol.Envelope{}, fmt.Errorf("timeout waiting for %s response", responseType)
	}
}

func (b *Bridge) readPump() {
	for {
		_, message, err := b.conn.ReadMessage()
		if err != nil {
			slog.Error("websocket read error", "error", err)
			os.Exit(1)
		}

		var env protocol.Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			continue
		}

		b.pendingMu.Lock()
		ch, ok := b.pending[env.Type]
		b.pendingMu.Unlock()

		if ok {
			select {
			case ch <- env:
			default:
			}
			continue
		}

		if env.Type == protocol.TypeScratchpadUpdate || env.Type == protocol.TypeScratchpadResult {
			b.pendingMu.Lock()
			ch, ok = b.pending[protocol.TypeScratchpadResult]
			b.pendingMu.Unlock()
			if ok {
				select {
				case ch <- env:
				default:
				}
				continue
			}
		}

		if env.From != "server" || env.Type != protocol.TypeLeaderInfo {
			b.incMu.Lock()
			b.incoming = append(b.incoming, env)
			b.incMu.Unlock()
		}
	}
}

func registerTools(s *mcp.Server, b *Bridge) {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}

	s.RegisterTool(mcp.Tool{
		Name:        "send_message",
		Description: "Send a direct message to another agent in the session",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Target agent ID"},
				"payload": map[string]any{"type": "object", "description": "Message payload"},
			},
			"required": []string{"to", "payload"},
		},
	}, func(args map[string]any) (string, error) {
		to, _ := args["to"].(string)
		if to == "" {
			return "", fmt.Errorf("to is required")
		}
		env, _ := protocol.NewEnvelope(protocol.TypeMessage, b.sessionID, b.agentID, to, args["payload"])
		return "sent", b.sendWS(env)
	})

	s.RegisterTool(mcp.Tool{
		Name:        "broadcast",
		Description: "Broadcast a message to all agents in the session",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"payload": map[string]any{"type": "object", "description": "Message payload"},
			},
			"required": []string{"payload"},
		},
	}, func(args map[string]any) (string, error) {
		env, _ := protocol.NewEnvelope(protocol.TypeBroadcast, b.sessionID, b.agentID, "", args["payload"])
		return "sent", b.sendWS(env)
	})

	s.RegisterTool(mcp.Tool{
		Name:        "receive_messages",
		Description: "Return all queued incoming messages (direct messages, broadcasts, task messages, notifications) since the last call. Call this periodically to process incoming agent communication.",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		b.incMu.Lock()
		msgs := b.incoming
		b.incoming = nil
		b.incMu.Unlock()
		if len(msgs) == 0 {
			return "[]", nil
		}
		data, _ := json.Marshal(msgs)
		return string(data), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "list_agents",
		Description: "List all agents currently connected to the session",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		resp, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeListAgents}, protocol.TypeAgentsList, 5*time.Second)
		if err != nil {
			return "", err
		}
		return string(resp.Payload), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "get_leader",
		Description: "Get the current leader agent ID for the session",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		resp, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeLeaderQuery}, protocol.TypeLeaderInfo, 5*time.Second)
		if err != nil {
			return "", err
		}
		return string(resp.Payload), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "transfer_leadership",
		Description: "Transfer session leadership to another agent (only current leader can do this)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"new_leader_id": map[string]any{"type": "string", "description": "Agent ID to transfer leadership to"},
			},
			"required": []string{"new_leader_id"},
		},
	}, func(args map[string]any) (string, error) {
		newLeader, _ := args["new_leader_id"].(string)
		payload, _ := json.Marshal(protocol.LeaderTransferPayload{NewLeaderID: newLeader})
		_, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeLeaderTransfer, Payload: payload}, protocol.TypeLeaderInfo, 5*time.Second)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("leadership transferred to %s", newLeader), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "scratchpad_set",
		Description: "Set a key-value pair in the shared session scratchpad. Other agents are notified of the change.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":   map[string]any{"type": "string", "description": "Key name"},
				"value": map[string]any{"description": "Value to store (any JSON type)"},
			},
			"required": []string{"key", "value"},
		},
	}, func(args map[string]any) (string, error) {
		key, _ := args["key"].(string)
		if key == "" {
			return "", fmt.Errorf("key is required")
		}
		valBytes, _ := json.Marshal(args["value"])
		payload, _ := json.Marshal(protocol.ScratchpadSetPayload{Key: key, Value: valBytes})
		resp, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeScratchpadSet, Payload: payload}, protocol.TypeScratchpadResult, 5*time.Second)
		if err != nil {
			return "", err
		}
		return string(resp.Payload), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "scratchpad_get",
		Description: "Get a value from the shared session scratchpad by key",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string", "description": "Key name"},
			},
			"required": []string{"key"},
		},
	}, func(args map[string]any) (string, error) {
		key, _ := args["key"].(string)
		payload, _ := json.Marshal(map[string]string{"key": key})
		resp, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeScratchpadGet, Payload: payload}, protocol.TypeScratchpadResult, 5*time.Second)
		if err != nil {
			return "", err
		}
		return string(resp.Payload), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "scratchpad_delete",
		Description: "Delete a key from the shared session scratchpad",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string", "description": "Key name"},
			},
			"required": []string{"key"},
		},
	}, func(args map[string]any) (string, error) {
		key, _ := args["key"].(string)
		payload, _ := json.Marshal(map[string]string{"key": key})
		resp, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeScratchpadDelete, Payload: payload}, protocol.TypeScratchpadResult, 5*time.Second)
		if err != nil {
			return "", err
		}
		return string(resp.Payload), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "scratchpad_list",
		Description: "List all key-value entries in the shared session scratchpad",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		resp, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeScratchpadList}, protocol.TypeScratchpadResult, 5*time.Second)
		if err != nil {
			return "", err
		}
		return string(resp.Payload), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "task_assign",
		Description: "Assign a task to another agent in the session",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":          map[string]any{"type": "string", "description": "Target agent ID"},
				"task_id":     map[string]any{"type": "string", "description": "Unique task identifier"},
				"description": map[string]any{"type": "string", "description": "Task description"},
				"parameters":  map[string]any{"description": "Optional task parameters (any JSON)"},
			},
			"required": []string{"to", "task_id", "description"},
		},
	}, func(args map[string]any) (string, error) {
		to, _ := args["to"].(string)
		taskID, _ := args["task_id"].(string)
		desc, _ := args["description"].(string)
		var paramsJSON json.RawMessage
		if p, ok := args["parameters"]; ok {
			paramsJSON, _ = json.Marshal(p)
		}
		payload, _ := json.Marshal(protocol.TaskAssignPayload{
			TaskID: taskID, Description: desc, Parameters: paramsJSON,
		})
		env := protocol.Envelope{Type: protocol.TypeTaskAssign, To: to, Payload: payload}
		return "sent", b.sendWS(env)
	})

	s.RegisterTool(mcp.Tool{
		Name:        "task_status",
		Description: "Update the status of a task and notify the relevant agent",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Target agent ID"},
				"task_id": map[string]any{"type": "string", "description": "Task identifier"},
				"status":  map[string]any{"type": "string", "description": "Status (e.g. in_progress, completed, failed)"},
				"detail":  map[string]any{"type": "string", "description": "Optional detail about the status"},
			},
			"required": []string{"to", "task_id", "status"},
		},
	}, func(args map[string]any) (string, error) {
		to, _ := args["to"].(string)
		taskID, _ := args["task_id"].(string)
		status, _ := args["status"].(string)
		detail, _ := args["detail"].(string)
		payload, _ := json.Marshal(protocol.TaskStatusPayload{TaskID: taskID, Status: status, Detail: detail})
		env := protocol.Envelope{Type: protocol.TypeTaskStatus, To: to, Payload: payload}
		return "sent", b.sendWS(env)
	})

	s.RegisterTool(mcp.Tool{
		Name:        "task_result",
		Description: "Return the result of a completed task to the requesting agent",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Target agent ID"},
				"task_id": map[string]any{"type": "string", "description": "Task identifier"},
				"result":  map[string]any{"description": "Task result (any JSON)"},
			},
			"required": []string{"to", "task_id", "result"},
		},
	}, func(args map[string]any) (string, error) {
		to, _ := args["to"].(string)
		taskID, _ := args["task_id"].(string)
		resultJSON, _ := json.Marshal(args["result"])
		payload, _ := json.Marshal(protocol.TaskResultPayload{TaskID: taskID, Result: resultJSON})
		env := protocol.Envelope{Type: protocol.TypeTaskResult, To: to, Payload: payload}
		return "sent", b.sendWS(env)
	})

	s.RegisterTool(mcp.Tool{
		Name:        "request_history",
		Description: "Request message history from the session, optionally after a given sequence number for catch-up after reconnection",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"after_sequence": map[string]any{"type": "integer", "description": "Only return messages after this sequence number"},
				"limit":          map[string]any{"type": "integer", "description": "Maximum messages to return (default 100)"},
			},
		},
	}, func(args map[string]any) (string, error) {
		afterSeq, _ := args["after_sequence"].(float64)
		limit, _ := args["limit"].(float64)
		payload, _ := json.Marshal(protocol.HistoryRequestPayload{
			AfterSequence: int64(afterSeq),
			Limit:         int(limit),
		})
		resp, err := b.sendAndWait(protocol.Envelope{Type: protocol.TypeHistoryRequest, Payload: payload}, protocol.TypeHistoryResult, 5*time.Second)
		if err != nil {
			return "", err
		}
		return string(resp.Payload), nil
	})
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "required environment variable %s is not set\n", key)
		os.Exit(1)
	}
	return v
}

func envOrDefault(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
