package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/signal"
)

// lastSeq tracks the highest sequence number we've seen, to avoid
// re-processing old messages on SSE reconnect.
var lastSeq atomic.Int64

// pendingSignal holds metadata about the message(s) that triggered the last signal,
// so we can include contextual info when the agent calls receive_messages or wait_for_message.
var pendingSignal atomic.Pointer[pendingSignalInfo]

type pendingSignalInfo struct {
	TriggerType string `json:"trigger_type"` // "message", "broadcast", "task_assign", etc.
	FromAgent   string `json:"from_agent"`
	Sequence    int64  `json:"sequence"`
}

// startWatcher connects to the server's SSE /watch endpoint and sends
// a check_messages signal to picobot whenever a message arrives for this agent.
func (b *Bridge) startWatcher(ctx context.Context) {
	if b.signalSocketPath == "" {
		slog.Info("watcher: no signal socket configured, skipping SSE watch")
		return
	}

	// Register with the server immediately so our mailbox exists
	// before we start watching for incoming messages.
	if err := b.ensureInit(); err != nil {
		slog.Error("watcher: failed to register with server on startup", "error", err)
		// Continue anyway — tools will retry registration on demand
	}

	// Initialize lastSeq from current history so we skip stale messages on startup.
	b.initLastSeq(ctx)

	// Start presence heartbeat to keep agent visible in list_agents.
	// The server's presence tracker has a 60s TTL, so we refresh every 30s.
	go b.presenceHeartbeat(ctx)

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

// presenceHeartbeat periodically sends a registration request to keep
// the agent's presence alive. The server expires agents after 60s of
// inactivity (presence TTL), so we refresh every 30s.
// This does NOT reset the initialized flag — it directly calls the
// register endpoint to touch presence without affecting MCP tool state.
func (b *Bridge) presenceHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.touchPresence(ctx); err != nil {
				slog.Warn("watcher: presence heartbeat failed", "error", err)
			}
		}
	}
}

// touchPresence sends a registration request to refresh the agent's
// presence TTL on the server. Unlike ensureInit, this doesn't change
// the bridge's internal initialized state.
func (b *Bridge) touchPresence(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{
		"agent_name":   b.agentName,
		"capabilities": b.capabilities,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		b.httpBase+"/sessions/"+b.sessionID+"/register", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.psk)
	req.Header.Set("X-Agent-ID", b.agentID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rbody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(rbody))
	}

	slog.Debug("watcher: presence heartbeat ok")
	return nil
}

// initLastSeq fetches the current session history and sets lastSeq to the
// highest sequence number found. This ensures that on startup/reconnect,
// only genuinely new messages trigger signals.
func (b *Bridge) initLastSeq(ctx context.Context) {
	type histEntry struct {
		Sequence int64 `json:"sequence"`
	}

	url := fmt.Sprintf("%s/sessions/%s/history?limit=50", b.httpBase, b.sessionID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		slog.Warn("watcher: failed to create history request", "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.psk)
	req.Header.Set("X-Agent-ID", b.agentID)

	resp, err := b.client.Do(req)
	if err != nil {
		slog.Warn("watcher: failed to fetch history for lastSeq init", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("watcher: history fetch returned non-200", "status", resp.StatusCode)
		return
	}

	// The API returns a flat JSON array of envelopes, not a wrapped object.
	var hist []histEntry
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		slog.Warn("watcher: failed to decode history", "error", err)
		return
	}

	var maxSeq int64
	for _, m := range hist {
		if m.Sequence > maxSeq {
			maxSeq = m.Sequence
		}
	}

	lastSeq.Store(maxSeq)
	slog.Info("watcher: initialized lastSeq from history", "lastSeq", maxSeq)
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

	// Skip messages we've already seen (stale replay on reconnect)
	current := lastSeq.Load()
	if env.Sequence > 0 && env.Sequence <= current {
		slog.Debug("watcher: skipping stale message", "seq", env.Sequence, "lastSeq", current)
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

	// Update lastSeq to the latest sequence we've processed
	if env.Sequence > 0 {
		lastSeq.Store(env.Sequence)
	}

	// Determine a human-friendly trigger type label
	triggerType := env.Type
	if env.Type == "message" && env.To == "*" {
		triggerType = "broadcast"
	}

	slog.Info("watcher: relevant message detected, sending check_messages signal",
		"type", env.Type,
		"from", env.From,
		"to", env.To,
		"sequence", env.Sequence,
		"trigger_type", triggerType,
	)

	// Store pending signal info so receive_messages/wait_for_message can
	// report what triggered the signal
	pendingSignal.Store(&pendingSignalInfo{
		TriggerType: triggerType,
		FromAgent:   env.From,
		Sequence:    env.Sequence,
	})

	sig := signal.Signal{
		Source: "agentchat-mcp",
		Action: "check_messages",
		Metadata: map[string]interface{}{
			"trigger_type": triggerType,
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

// getPendingSignalInfo returns and clears the pending signal metadata.
func getPendingSignalInfo() *pendingSignalInfo {
	return pendingSignal.Swap(nil)
}
