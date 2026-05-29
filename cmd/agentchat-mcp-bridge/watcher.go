package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/signal"
)

// startWatcher connects to the server's SSE /watch endpoint and sends
// a check_messages signal to picobot whenever a message arrives for this agent.
func (b *Bridge) startWatcher(ctx context.Context) {
	if b.signalSocketPath == "" {
		slog.Info("watcher: no signal socket configured, skipping SSE watch")
		return
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := b.watchStream(ctx)
			if err != nil {
				slog.Error("watcher: SSE stream error, reconnecting in 5s", "error", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}
	}()
}

func (b *Bridge) watchStream(ctx context.Context) error {
	url := fmt.Sprintf("%s/watch?session=%s&psk=%s", b.httpBase, b.sessionID, b.psk)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// Use sseClient (no timeout) instead of b.client (30s timeout)
	resp, err := b.sseClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	slog.Info("watcher: connected to SSE stream")

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType, eventData string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			eventData = strings.TrimPrefix(line, "data: ")
			continue
		}

		// Empty line = end of event
		if line == "" && eventData != "" {
			b.handleSSEEvent(eventType, eventData)
			eventType = ""
			eventData = ""
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stream read: %w", err)
	}

	return fmt.Errorf("stream ended")
}

type sseEnvelope struct {
	Type     string `json:"type"`
	From     string `json:"from"`
	To       string `json:"to"`
	Sequence int64  `json:"sequence"`
}

func (b *Bridge) handleSSEEvent(sseEventType, data string) {
	// Only process live "message" SSE events, skip history replays and system events
	if sseEventType != "message" {
		return
	}

	var env sseEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		slog.Debug("watcher: failed to parse SSE data", "error", err)
		return
	}

	// Only signal on messages directed to us or broadcasts
	switch env.Type {
	case "message":
		if env.To == "*" {
			// Broadcast delivered as "message" type with To=*
			if env.From == b.agentID {
				return // our own broadcast, skip
			}
		} else if env.To != b.agentID {
			return // not for us
		}
	case "broadcast":
		if env.From == b.agentID {
			return // our own broadcast, skip
		}
	case "task_assign", "task_status", "task_result", "file_share":
		// These are message types we want to know about
		if env.To != b.agentID {
			return
		}
	case "agent_joined", "agent_left":
		// Interesting but not something we need to signal about
		return
	default:
		return
	}

	slog.Info("watcher: relevant message detected, sending check_messages signal",
		"type", env.Type,
		"from", env.From,
		"to", env.To,
		"sequence", env.Sequence,
	)

	sig := signal.Signal{
		Source: "agentchat-mcp",
		Action: "check_messages",
		Metadata: map[string]interface{}{
			"trigger_type": env.Type,
			"from_agent":   env.From,
			"sequence":     env.Sequence,
		},
	}

	resp, err := signal.SendToSocket(b.signalSocketPath, sig)
	if err != nil {
		slog.Error("watcher: failed to send signal", "error", err)
		return
	}

	slog.Info("watcher: signal sent", "response", resp)
}
