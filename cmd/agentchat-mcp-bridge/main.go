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

	"github.com/wltechblog/agentchat-mcp/internal/mcp"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/signal"
)

type Bridge struct {
	httpBase     string
	sessionID    string
	psk          string
	agentID      string
	agentName    string
	capabilities []string
	client       *http.Client
	sseClient    *http.Client

	mu                sync.Mutex
	initialized       bool
	debugLog          bool
	signalSocketPath  string // path to picobot's Unix socket (local)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	serverURL := requireEnv("AGENTCHAT_URL")
	sessionID := requireEnv("AGENTCHAT_SESSION_ID")
	psk := requireEnv("AGENTCHAT_PSK")
	agentID := requireEnv("AGENTCHAT_AGENT_ID")
	agentName := envOrDefault("AGENTCHAT_AGENT_NAME", agentID)
	capsStr := envOrDefault("AGENTCHAT_CAPABILITIES", "")
	debugStr := envOrDefault("AGENTCHAT_DEBUG", "")
	var debugLog bool
	if strings.ToLower(debugStr) == "true" || debugStr == "1" {
		debugLog = true
	}

	// Signal socket path — prefer PICOBOT_SIGNAL_SOCKET (auto-injected by picobot),
	// fall back to AGENTCHAT_SIGNAL_SOCKET for manual config
	signalSocketPath := envOrDefault("PICOBOT_SIGNAL_SOCKET", envOrDefault("AGENTCHAT_SIGNAL_SOCKET", ""))

	var caps []string
	if capsStr != "" {
		caps = strings.Split(capsStr, ",")
		for i := range caps {
			caps[i] = strings.TrimSpace(caps[i])
		}
	}

	httpBase := serverURL
	httpBase = strings.TrimSuffix(httpBase, "/")
	httpBase = strings.TrimSuffix(httpBase, "/ws")
	if strings.HasPrefix(httpBase, "wss://") {
		httpBase = "https://" + strings.TrimPrefix(httpBase, "wss://")
	} else if strings.HasPrefix(httpBase, "ws://") {
		httpBase = "http://" + strings.TrimPrefix(httpBase, "ws://")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridge := &Bridge{
		httpBase:         httpBase,
		sessionID:        sessionID,
		psk:              psk,
		agentID:          agentID,
		agentName:        agentName,
		capabilities:     caps,
		client:           &http.Client{Timeout: 30 * time.Second},
	sseClient:        &http.Client{},
		debugLog:         debugLog,
		signalSocketPath: signalSocketPath,
	}

	server := mcp.NewServer("agentchat-mcp-bridge", "1.3.0")
	registerTools(server, bridge)

	slog.Info("bridge started", "agent_id", agentID, "session_id", sessionID, "server", httpBase)

	// Start SSE watcher to auto-signal picobot on incoming messages
	bridge.startWatcher(ctx)
	if signalSocketPath != "" {
		slog.Info("signal socket configured", "path", signalSocketPath)
	}
	if err := server.Run(ctx); err != nil {
		slog.Error("MCP server exited", "error", err)
	}
}

func (b *Bridge) ensureInit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initialized {
		return nil
	}

	body, _ := json.Marshal(map[string]any{
		"agent_name":   b.agentName,
		"capabilities": b.capabilities,
	})
	resp, err := b.doRequestLocked("POST", "/sessions/"+b.sessionID+"/register", body)
	if err != nil {
		return fmt.Errorf("register failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rbody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register failed (%d): %s", resp.StatusCode, string(rbody))
	}

	b.initialized = true
	slog.Info("registered with server")
	return nil
}

func (b *Bridge) doRequest(method, path string, body []byte) (*http.Response, error) {
	if err := b.ensureInit(); err != nil {
		return nil, err
	}

	b.mu.Lock()
	resp, err := b.doRequestLocked(method, path, body)
	b.mu.Unlock()
	return resp, err
}

func (b *Bridge) doRequestLocked(method, path string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, b.httpBase+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.psk)
	req.Header.Set("X-Agent-ID", b.agentID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if b.debugLog {
		slog.Info("HTTP request", "method", method, "path", path)
	}
	return b.client.Do(req)
}

func (b *Bridge) doJSON(method, path string, payload any) (any, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}

	resp, err := b.doRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	rbody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, string(rbody))
	}

	var result any
	json.Unmarshal(rbody, &result)
	return result, nil
}

func (b *Bridge) drainMailbox() ([]map[string]any, error) {
	result, err := b.doJSON("GET", "/sessions/"+b.sessionID+"/mailbox", nil)
	if err != nil {
		return nil, err
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, nil
	}

	rawMsgs, _ := resultMap["messages"].([]any)
	msgs := make([]map[string]any, 0, len(rawMsgs))
	for _, m := range rawMsgs {
		if m, ok := m.(map[string]any); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

func registerTools(s *mcp.Server, b *Bridge) {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}

	s.RegisterTool(mcp.Tool{
		Name:        "send_message",
		Description: "Send a direct message to another agent in the session. The remote agent may take time to process and respond; use wait_for_message to block until a reply arrives.",
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
		_, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/messages", map[string]any{
			"to":      to,
			"type":    "message",
			"payload": args["payload"],
		})
		if err != nil {
			return "", err
		}
		return "sent", nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "broadcast",
		Description: "Broadcast a message to all agents in the session. Remote agents may take time to respond; use wait_for_message or receive_messages to collect replies.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"payload": map[string]any{"type": "object", "description": "Message payload"},
			},
			"required": []string{"payload"},
		},
	}, func(args map[string]any) (string, error) {
		_, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/broadcast", map[string]any{
			"type":    "broadcast",
			"payload": args["payload"],
		})
		if err != nil {
			return "", err
		}
		return "sent", nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "receive_messages",
		Description: "Return all queued incoming messages (direct messages, broadcasts, task messages, notifications) since the last call. Returns immediately with whatever is available. For blocking until a message arrives, use wait_for_message instead.",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		msgs, err := b.drainMailbox()
		if err != nil {
			return "", err
		}
		if len(msgs) == 0 {
			return "[]", nil
		}
		data, _ := json.Marshal(msgs)
		return string(data), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "wait_for_message",
		Description: "Block until one or more incoming messages arrive, then return them. This avoids repeated polling when waiting for a response from a remote agent which may take seconds or minutes to reply. Checks existing queued messages first, then polls up to the specified timeout.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"timeout": map[string]any{"type": "number", "description": "Maximum seconds to wait (default 120, max 600)"},
				"from":    map[string]any{"type": "string", "description": "Only return messages from this agent ID"},
				"type":    map[string]any{"type": "string", "description": "Only return messages of this type (e.g. message, task_status, task_result, broadcast)"},
			},
		},
	}, func(args map[string]any) (string, error) {
		timeoutSec, _ := args["timeout"].(float64)
		if timeoutSec <= 0 {
			timeoutSec = 120
		}
		if timeoutSec > 600 {
			timeoutSec = 600
		}
		timeout := time.Duration(timeoutSec * float64(time.Second))

		filterFrom, _ := args["from"].(string)
		filterType, _ := args["type"].(string)

		deadline := time.Now().Add(timeout)
		pollInterval := 2 * time.Second

		for {
			msgs, err := b.drainMailbox()
			if err != nil {
				return "", err
			}

			if filterFrom == "" && filterType == "" {
				if len(msgs) > 0 {
					data, _ := json.Marshal(msgs)
					return string(data), nil
				}
			} else {
				var matched, remaining []map[string]any
				for _, m := range msgs {
					env := m["envelope"]
					if envMap, ok := env.(map[string]any); ok {
						from, _ := envMap["from"].(string)
						t, _ := envMap["type"].(string)
						if (filterFrom == "" || from == filterFrom) && (filterType == "" || t == filterType) {
							matched = append(matched, m)
							continue
						}
					}
					remaining = append(remaining, m)
				}

				if len(matched) > 0 {
					data, _ := json.Marshal(matched)
					return string(data), nil
				}

				if len(remaining) > 0 {
					slog.Warn("wait_for_message: non-matching messages discarded by destructive read", "count", len(remaining))
				}
			}

			remaining := time.Until(deadline)
			if remaining <= 0 {
				return "[]", nil
			}

			sleep := pollInterval
			if remaining < sleep {
				sleep = remaining
			}
			time.Sleep(sleep)
		}
	})

	s.RegisterTool(mcp.Tool{
		Name:        "send_and_wait",
		Description: "Send a message to another agent and block until a reply arrives. Combines send_message + wait_for_message into a single synchronous call.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Target agent ID"},
				"payload": map[string]any{"type": "object", "description": "Message payload"},
				"timeout": map[string]any{"type": "number", "description": "Maximum seconds to wait for reply (default 120, max 600)"},
			},
			"required": []string{"to", "payload"},
		},
	}, func(args map[string]any) (string, error) {
		to, _ := args["to"].(string)
		if to == "" {
			return "", fmt.Errorf("to is required")
		}

		timeoutSec, _ := args["timeout"].(float64)
		if timeoutSec <= 0 {
			timeoutSec = 120
		}
		if timeoutSec > 600 {
			timeoutSec = 600
		}
		timeout := time.Duration(timeoutSec * float64(time.Second))

		_, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/messages", map[string]any{
			"to":      to,
			"type":    "message",
			"payload": args["payload"],
		})
		if err != nil {
			return "", fmt.Errorf("send failed: %w", err)
		}

		deadline := time.Now().Add(timeout)
		pollInterval := 2 * time.Second

		for {
			msgs, err := b.drainMailbox()
			if err != nil {
				return "", err
			}

			var matched []map[string]any
			for _, m := range msgs {
				if env, ok := m["envelope"].(map[string]any); ok {
					if from, _ := env["from"].(string); from == to {
						matched = append(matched, m)
					}
				}
			}

			if len(matched) > 0 {
				data, _ := json.Marshal(matched)
				return string(data), nil
			}

			remaining := time.Until(deadline)
			if remaining <= 0 {
				return "[]", nil
			}

			sleep := pollInterval
			if remaining < sleep {
				sleep = remaining
			}
			time.Sleep(sleep)
		}
	})

	s.RegisterTool(mcp.Tool{
		Name:        "list_agents",
		Description: "List all agents currently connected to the session",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		result, err := b.doJSON("GET", "/sessions/"+b.sessionID+"/agents", nil)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "get_leader",
		Description: "Get the current leader agent ID for the session",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		result, err := b.doJSON("GET", "/sessions/"+b.sessionID+"/leader", nil)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
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
		_, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/leader/transfer", map[string]any{
			"new_leader_id": newLeader,
		})
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
		key := stringifyArg(args["key"])
		if key == "" {
			return "", fmt.Errorf("key is required")
		}
		result, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/scratchpad/set", map[string]any{
			"key":   key,
			"value": args["value"],
		})
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
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
		key := stringifyArg(args["key"])
		if key == "" {
			return "", fmt.Errorf("key is required")
		}
		result, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/scratchpad/get", map[string]any{
			"key": key,
		})
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
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
		key := stringifyArg(args["key"])
		if key == "" {
			return "", fmt.Errorf("key is required")
		}
		result, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/scratchpad/delete", map[string]any{
			"key": key,
		})
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "scratchpad_list",
		Description: "List all key-value entries in the shared session scratchpad",
		InputSchema: empty,
	}, func(args map[string]any) (string, error) {
		result, err := b.doJSON("GET", "/sessions/"+b.sessionID+"/scratchpad", nil)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "task_assign",
		Description: "Assign a task to another agent in the session. The remote agent may take minutes to complete the task; use wait_for_message to block until a task_status or task_result response arrives.",
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
		if to == "" {
			return "", fmt.Errorf("to is required")
		}
		taskPayload, _ := json.Marshal(map[string]any{
			"task_id":     args["task_id"],
			"description": args["description"],
			"parameters":  args["parameters"],
		})
		_, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/messages", map[string]any{
			"to":      to,
			"type":    "task_assign",
			"payload": json.RawMessage(taskPayload),
		})
		if err != nil {
			return "", err
		}
		return "sent", nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "task_status",
		Description: "Update the status of a task and notify the relevant agent.",
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
		if to == "" {
			return "", fmt.Errorf("to is required")
		}
		taskPayload, _ := json.Marshal(map[string]any{
			"task_id": args["task_id"],
			"status":  args["status"],
			"detail":  args["detail"],
		})
		_, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/messages", map[string]any{
			"to":      to,
			"type":    "task_status",
			"payload": json.RawMessage(taskPayload),
		})
		if err != nil {
			return "", err
		}
		return "sent", nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "task_result",
		Description: "Return the result of a completed task to the requesting agent.",
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
		if to == "" {
			return "", fmt.Errorf("to is required")
		}
		taskPayload, _ := json.Marshal(map[string]any{
			"task_id": args["task_id"],
			"result":  args["result"],
		})
		_, err := b.doJSON("POST", "/sessions/"+b.sessionID+"/messages", map[string]any{
			"to":      to,
			"type":    "task_result",
			"payload": json.RawMessage(taskPayload),
		})
		if err != nil {
			return "", err
		}
		return "sent", nil
	})

	s.RegisterTool(mcp.Tool{
		Name:        "request_history",
		Description: "Request message history from the session, optionally after a given sequence number for catch-up",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"after_sequence": map[string]any{"type": "integer", "description": "Only return messages after this sequence number"},
				"limit":          map[string]any{"type": "integer", "description": "Maximum messages to return (default 100)"},
			},
		},
	}, func(args map[string]any) (string, error) {
		path := "/sessions/" + b.sessionID + "/history"
		params := []string{}
		if afterSeq, ok := args["after_sequence"].(float64); ok && afterSeq > 0 {
			params = append(params, fmt.Sprintf("after_sequence=%d", int64(afterSeq)))
		}
		if limit, ok := args["limit"].(float64); ok && limit > 0 {
			params = append(params, fmt.Sprintf("limit=%d", int(limit)))
		}
		if len(params) > 0 {
			path += "?" + strings.Join(params, "&")
		}

		result, err := b.doJSON("GET", path, nil)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
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

		if err := b.ensureInit(); err != nil {
			return "", err
		}

		uploadURL := fmt.Sprintf("%s/sessions/%s/files?filename=%s", b.httpBase, b.sessionID, filename)
		req, _ := http.NewRequest("POST", uploadURL, bytes.NewReader(data))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+b.psk)
		req.Header.Set("X-Agent-ID", b.agentID)
		resp, err := b.client.Do(req)
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
		_, err = b.doJSON("POST", "/sessions/"+b.sessionID+"/messages", map[string]any{
			"to":      to,
			"type":    "file_share",
			"payload": json.RawMessage(sharePayload),
		})
		if err != nil {
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

		if err := b.ensureInit(); err != nil {
			return "", err
		}

		dlURL := fmt.Sprintf("%s/sessions/%s/files/%s", b.httpBase, b.sessionID, fileID)
		req, _ := http.NewRequest("GET", dlURL, nil)
		req.Header.Set("Authorization", "Bearer "+b.psk)
		req.Header.Set("X-Agent-ID", b.agentID)
		resp, err := b.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("download failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("download failed (%d)", resp.StatusCode)
		}

		fdata, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("read failed: %w", err)
		}

		result, _ := json.Marshal(map[string]any{
			"file_id":        fileID,
			"content_type":   resp.Header.Get("Content-Type"),
			"size":           len(fdata),
			"content_base64": base64.StdEncoding.EncodeToString(fdata),
		})
		return string(result), nil
	})

	// trigger_agent — send an action-based signal to picobot's local Unix socket
	s.RegisterTool(mcp.Tool{
		Name:        "trigger_agent",
		Description: "Send a trigger signal to a local picobot agent instance via Unix socket. The signal carries a registered action name — picobot validates the action and injects a safe, pre-defined response. This wakes the agent to perform a known task (check messages, check email, etc). Requires PICOBOT_SIGNAL_SOCKET to be configured (auto-injected by picobot).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "description": "The registered action to trigger (e.g., check_messages, motion_detected). Must be a known action registered in picobot's config."},
				"channel": map[string]any{"type": "string", "description": "Target channel (e.g., telegram, discord). Leave empty for default."},
				"chat_id": map[string]any{"type": "string", "description": "Target chat ID. Leave empty for default."},
			},
			"required": []string{"action"},
		},
	}, func(args map[string]any) (string, error) {
		action, _ := args["action"].(string)
		if action == "" {
			return "", fmt.Errorf("action is required")
		}

		if b.signalSocketPath == "" {
			return "", fmt.Errorf("trigger_agent not configured: PICOBOT_SIGNAL_SOCKET not set. Ensure picobot signal system is enabled and this bridge was spawned by picobot.")
		}

		sig := signal.Signal{
			Source:  "agentchat-mcp",
			Action:  action,
			Channel: maybeString(args["channel"]),
			ChatID:  maybeString(args["chat_id"]),
			Metadata: map[string]interface{}{
				"source_agent": b.agentID,
			},
		}

		resp, err := signal.SendToSocket(b.signalSocketPath, sig)
		if err != nil {
			return "", fmt.Errorf("trigger failed: %w", err)
		}

		data, _ := json.Marshal(resp)
		return string(data), nil
	})
}

func maybeString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func stringifyArg(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
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
