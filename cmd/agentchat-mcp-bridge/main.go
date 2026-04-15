package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wltechblog/agentchat-mcp/internal/mcp"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

type Bridge struct {
	wsURL        string
	sessionID    string
	psk          string
	agentID      string
	agentName    string
	capabilities []string
	httpBase     string

	connMu    sync.RWMutex
	conn      *websocket.Conn
	connected bool
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

	bridge := &Bridge{
		wsURL:        wsURL,
		sessionID:    sessionID,
		psk:          psk,
		agentID:      agentID,
		agentName:    agentName,
		capabilities: caps,
		httpBase:     wsURLToHTTP(wsURL),
		pending:      make(map[string]chan protocol.Envelope),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go bridge.connectLoop(ctx)

	server := mcp.NewServer("agentchat-mcp-bridge", "1.0.0")
	registerTools(server, bridge)

	slog.Info("bridge started", "agent_id", agentID, "session_id", sessionID)
	if err := server.Run(ctx); err != nil {
		slog.Error("MCP server exited", "error", err)
	}
}

func (b *Bridge) connectLoop(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := b.connect()
		if err != nil {
			slog.Error("connection failed", "error", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			case <-ctx.Done():
				return
			}
		}

		backoff = time.Second
		slog.Info("connected to server")

		b.readPump()

		b.disconnect()
		slog.Warn("disconnected, will reconnect", "retry_in", backoff)

		select {
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		case <-ctx.Done():
			return
		}
	}
}

func (b *Bridge) connect() error {
	conn, _, err := websocket.DefaultDialer.Dial(b.wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	authPayload, _ := json.Marshal(protocol.AuthPayload{
		SessionID:    b.sessionID,
		AgentID:      b.agentID,
		AgentName:    b.agentName,
		PSK:          b.psk,
		Capabilities: b.capabilities,
	})
	authMsg, _ := json.Marshal(protocol.Envelope{
		Type:      protocol.TypeAuth,
		Payload:   authPayload,
		Timestamp: time.Now().UTC(),
	})
	if err := conn.WriteMessage(websocket.TextMessage, authMsg); err != nil {
		conn.Close()
		return fmt.Errorf("auth write: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, resp, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("auth read: %w", err)
	}

	var authResp protocol.Envelope
	json.Unmarshal(resp, &authResp)
	if authResp.Type != protocol.TypeAuthOK {
		conn.Close()
		return fmt.Errorf("auth rejected: %s", string(resp))
	}
	conn.SetReadDeadline(time.Time{})

	b.connMu.Lock()
	b.conn = conn
	b.connected = true
	b.connMu.Unlock()

	return nil
}

func (b *Bridge) disconnect() {
	b.connMu.Lock()
	b.connected = false
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	b.connMu.Unlock()

	b.pendingMu.Lock()
	for k, ch := range b.pending {
		close(ch)
		delete(b.pending, k)
	}
	b.pendingMu.Unlock()
}

func (b *Bridge) isConnected() bool {
	b.connMu.RLock()
	defer b.connMu.RUnlock()
	return b.connected
}

func (b *Bridge) readPump() {
	for {
		b.connMu.RLock()
		conn := b.conn
		b.connMu.RUnlock()
		if conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error("read error", "error", err)
			}
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			continue
		}

		b.routeMessage(env)
	}
}

func (b *Bridge) routeMessage(env protocol.Envelope) {
	b.pendingMu.Lock()
	ch, ok := b.pending[env.Type]
	b.pendingMu.Unlock()

	if ok {
		select {
		case ch <- env:
		default:
		}
		return
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
			return
		}
	}

	if env.From != "server" || env.Type != protocol.TypeLeaderInfo {
		b.incMu.Lock()
		b.incoming = append(b.incoming, env)
		b.incMu.Unlock()
	}
}

func (b *Bridge) sendWS(env protocol.Envelope) error {
	b.connMu.RLock()
	conn := b.conn
	connected := b.connected
	b.connMu.RUnlock()

	if !connected {
		return fmt.Errorf("not connected to server")
	}

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
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (b *Bridge) sendAndWait(env protocol.Envelope, responseType string, timeout time.Duration) (protocol.Envelope, error) {
	if !b.isConnected() {
		return protocol.Envelope{}, fmt.Errorf("not connected to server")
	}

	ch := make(chan protocol.Envelope, 1)
	b.pendingMu.Lock()
	b.pending[responseType] = ch
	b.pendingMu.Unlock()

	if err := b.sendWS(env); err != nil {
		b.pendingMu.Lock()
		delete(b.pending, responseType)
		b.pendingMu.Unlock()
		return protocol.Envelope{}, err
	}

	select {
	case resp, ok := <-ch:
		b.pendingMu.Lock()
		delete(b.pending, responseType)
		b.pendingMu.Unlock()
		if !ok {
			return protocol.Envelope{}, fmt.Errorf("connection lost")
		}
		if resp.Type == protocol.TypeError {
			var errData map[string]string
			json.Unmarshal(resp.Payload, &errData)
			return protocol.Envelope{}, fmt.Errorf("%s", errData["error"])
		}
		return resp, nil
	case <-time.After(timeout):
		b.pendingMu.Lock()
		delete(b.pending, responseType)
		b.pendingMu.Unlock()
		return protocol.Envelope{}, fmt.Errorf("timeout waiting for %s response", responseType)
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

	s.RegisterTool(mcp.Tool{
		Name:        "send_file",
		Description: "Upload a file and share it with another agent. The file content is provided as base64. Returns file_id and shares it via the session.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":             map[string]any{"type": "string", "description": "Target agent ID"},
				"filename":       map[string]any{"type": "string", "description": "Name for the file"},
				"content_base64": map[string]any{"type": "string", "description": "Base64-encoded file content"},
				"content_type":   map[string]any{"type": "string", "description": "MIME type (default application/octet-stream)"},
				"description":    map[string]any{"type": "string", "description": "Optional description of the file"},
			},
			"required": []string{"to", "filename", "content_base64"},
		},
	}, func(args map[string]any) (string, error) {
		to, _ := args["to"].(string)
		filename, _ := args["filename"].(string)
		contentB64, _ := args["content_base64"].(string)
		contentType, _ := args["content_type"].(string)
		description, _ := args["description"].(string)

		if to == "" || filename == "" || contentB64 == "" {
			return "", fmt.Errorf("to, filename, and content_base64 are required")
		}

		data, err := base64.StdEncoding.DecodeString(contentB64)
		if err != nil {
			return "", fmt.Errorf("invalid base64: %w", err)
		}

		if contentType == "" {
			contentType = "application/octet-stream"
		}

		uploadURL := fmt.Sprintf("%s/sessions/%s/files?filename=%s", b.httpBase, b.sessionID, filename)
		req, _ := http.NewRequest("POST", uploadURL, bytes.NewReader(data))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Agent-ID", b.agentID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("upload failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("upload failed (%d): %s", resp.StatusCode, string(body))
		}

		var uploadResult map[string]any
		json.NewDecoder(resp.Body).Decode(&uploadResult)

		fileID, _ := uploadResult["file_id"].(string)
		size, _ := uploadResult["size"].(float64)

		sharePayload, _ := json.Marshal(protocol.FileSharePayload{
			FileID:      fileID,
			FileName:    filename,
			ContentType: contentType,
			Size:        int64(size),
			Description: description,
		})
		env := protocol.Envelope{Type: protocol.TypeFileShare, To: to, Payload: sharePayload}
		if err := b.sendWS(env); err != nil {
			return "", fmt.Errorf("file uploaded but share message failed: %w", err)
		}

		result, _ := json.Marshal(map[string]any{
			"file_id":   fileID,
			"file_name": filename,
			"size":      int64(size),
			"shared_to": to,
		})
		return string(result), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "download_file",
		Description: "Download a file by its file_id (obtained from a file_share message). Returns the file content as base64.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_id": map[string]any{"type": "string", "description": "File ID from the file_share message"},
			},
			"required": []string{"file_id"},
		},
	}, func(args map[string]any) (string, error) {
		fileID, _ := args["file_id"].(string)
		if fileID == "" {
			return "", fmt.Errorf("file_id is required")
		}

		dlURL := fmt.Sprintf("%s/sessions/%s/files/%s", b.httpBase, b.sessionID, fileID)
		resp, err := http.Get(dlURL)
		if err != nil {
			return "", fmt.Errorf("download failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("download failed (%d)", resp.StatusCode)
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("read failed: %w", err)
		}

		result, _ := json.Marshal(map[string]any{
			"file_id":        fileID,
			"content_type":   resp.Header.Get("Content-Type"),
			"size":           len(data),
			"content_base64": base64.StdEncoding.EncodeToString(data),
		})
		return string(result), nil
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

func wsURLToHTTP(wsURL string) string {
	u := strings.TrimSuffix(wsURL, "/ws")
	u = strings.TrimSuffix(u, "/")
	if strings.HasPrefix(u, "wss://") {
		return "https://" + strings.TrimPrefix(u, "wss://")
	}
	return "http://" + strings.TrimPrefix(u, "ws://")
}
