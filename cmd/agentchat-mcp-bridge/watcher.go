package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/signal"
)

// lastSeq tracks the highest sequence number we've seen on the SSE stream, so
// stale messages are skipped on reconnect.
var lastSeq atomic.Int64

// lastConnectSignal rate-limits the wake-up signal sent on every (re)connect.
var lastConnectSignal atomic.Int64

// pendingSignal holds metadata about the message(s) that triggered the last signal,
// so we can include contextual info when the agent calls receive_messages or wait_for_message.
var pendingSignal atomic.Pointer[pendingSignalInfo]

type pendingSignalInfo struct {
	TriggerType string `json:"trigger_type"` // "message", "broadcast", "task_assign", etc.
	FromAgent   string `json:"from_agent"`
	Sequence    int64  `json:"sequence"`
}

const (
	// SSE reconnect backoff: starts at sseBaseBackoff and doubles (with
	// jitter) up to sseMaxBackoff. A stream that stayed up sseHealthyUptime
	// counts as healthy and resets the curve.
	sseBaseBackoff   = 500 * time.Millisecond
	sseMaxBackoff    = 30 * time.Second
	sseHealthyUptime = 30 * time.Second

	// The server pings idle SSE streams every 15s. If nothing at all arrives
	// for sseWatchdogTimeout the connection is dead (a half-open TCP
	// connection is indistinguishable from an idle one), so we force a
	// reconnect instead of blocking on it forever.
	sseWatchdogTimeout = 45 * time.Second
	sseWatchdogTick    = 5 * time.Second

	// Wake-up signal send retries use the same capped exponential backoff.
	signalBaseBackoff = 500 * time.Millisecond
	signalMaxBackoff  = 30 * time.Second

	// Minimum gap between wake-up signals triggered by stream (re)connects,
	// so a flapping stream can't spam picobot. Message-triggered signals are
	// never rate-limited.
	reconnectSignalMinGap = 10 * time.Second
)

// startWatcher runs the bridge's server-facing background loops.
//
// Two things happen for EVERY bridge, regardless of signal-socket config:
//  1. Registration at startup, so the agent is visible to peers and its
//     mailbox exists before anyone tries to message it.
//  2. A presence heartbeat every 30s — without it the agent expires after
//     the server's presence TTL (60s) and appears offline.
//
// The SSE watch loop (which requests picobot signals on incoming messages)
// and the signal delivery loop only run when a signal socket is configured.
func (b *Bridge) startWatcher(ctx context.Context) {
	// Register with the server immediately so our mailbox exists
	// before peers try to message us.
	if err := b.ensureInit(); err != nil {
		slog.Error("watcher: failed to register with server on startup", "error", err)
		// Continue anyway — tools will retry registration on demand
	}

	// Start presence heartbeat to keep the agent visible in list_agents.
	go b.presenceHeartbeat(ctx)

	if b.signalSocketPath == "" {
		slog.Info("watcher: no signal socket configured, skipping SSE watch")
		return
	}

	go b.signalLoop(ctx)
	go b.sseLoop(ctx)
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

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// Server doesn't know us (restart); re-register on the next attempt.
		b.invalidateRegistration()
		return fmt.Errorf("heartbeat unauthorized; registration invalidated")
	case resp.StatusCode != http.StatusOK:
		rbody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(rbody))
	}

	// Each register renews the short-lived watch token.
	var regResp struct {
		WatchToken string `json:"watch_token"`
	}
	json.NewDecoder(resp.Body).Decode(&regResp)
	b.setWatchToken(regResp.WatchToken)

	slog.Debug("watcher: presence heartbeat ok")
	return nil
}

// initLastSeq fetches the current session history and sets lastSeq to the
// highest sequence number found. Called on every (re)connect: after a server
// restart a recreated session starts sequence numbers from 1 again, and
// without this re-sync every live event would be skipped as stale.
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

// sseLoop keeps a watch connection alive for the lifetime of the bridge:
// reconnects with capped exponential backoff plus jitter, and resets the
// backoff after a stream that stayed up long enough to be considered healthy.
func (b *Bridge) sseLoop(ctx context.Context) {
	backoff := sseBaseBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		err := b.watchStream(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Error("watcher: SSE stream error", "error", err, "reconnect_in", backoff)
		}

		if time.Since(started) >= sseHealthyUptime {
			backoff = sseBaseBackoff
		} else {
			backoff = backoffJitter(backoff*2, sseMaxBackoff)
		}

		if !sleepCtx(ctx, backoff) {
			return
		}
	}
}

// watchStream connects to the server's SSE /watch endpoint and processes
// events until the stream ends. A watchdog cancels the stream if no bytes
// arrive for sseWatchdogTimeout, converting a silently-dead connection into
// a reconnect instead of a forever-blocked read.
func (b *Bridge) watchStream(ctx context.Context) error {
	// The watch token comes from register. Without one — fresh start, or
	// the server restarted and forgot everything — register first; that also
	// re-creates the session if needed.
	if b.getWatchToken() == "" {
		if err := b.ensureInit(); err != nil {
			return fmt.Errorf("register for watch token: %w", err)
		}
	}

	// Re-sync the stale-skip watermark before connecting.
	b.initLastSeq(ctx)

	// streamCtx is cancellable by the watchdog to force a reconnect.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	url := fmt.Sprintf("%s/watch?session=%s&token=%s", b.httpBase, b.sessionID, b.getWatchToken())

	req, err := http.NewRequestWithContext(streamCtx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// sseClient has no timeout: the stream is long-lived. Dead connections
	// are detected by the watchdog below, not by a request timeout.
	resp, err := b.sseClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// Token expired or server forgot it; re-register on the next attempt.
		b.invalidateRegistration()
		return fmt.Errorf("watch unauthorized; registration invalidated for retry")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	slog.Info("watcher: connected to SSE stream")

	// Wake the agent only if mail actually queued while we were offline
	// (rate-limited so a flapping stream doesn't spam the host).
	if last := time.Unix(0, lastConnectSignal.Load()); time.Since(last) >= reconnectSignalMinGap {
		if fired, err := b.maybeWakeOnConnect(); err == nil && fired {
			lastConnectSignal.Store(time.Now().UnixNano())
		}
	}

	lastRead := &atomic.Int64{}
	lastRead.Store(time.Now().UnixNano())
	body := &watchdogBody{ReadCloser: resp.Body, lastRead: lastRead}

	stopWatchdog := make(chan struct{})
	defer close(stopWatchdog)
	go b.watchdog(streamCtx, cancel, lastRead, stopWatchdog)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType, eventData string

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")

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

// watchdogBody tracks the last time data arrived so the watchdog can tell a
// live-but-idle stream from a dead connection.
type watchdogBody struct {
	io.ReadCloser
	lastRead *atomic.Int64
}

func (w *watchdogBody) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	if n > 0 {
		w.lastRead.Store(time.Now().UnixNano())
	}
	return n, err
}

func (b *Bridge) watchdog(streamCtx context.Context, cancel context.CancelFunc, lastRead *atomic.Int64, stop <-chan struct{}) {
	ticker := time.NewTicker(sseWatchdogTick)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-streamCtx.Done():
			return
		case <-ticker.C:
			idle := time.Since(time.Unix(0, lastRead.Load()))
			if idle > sseWatchdogTimeout {
				slog.Warn("watcher: SSE stream idle past watchdog timeout, forcing reconnect", "idle", idle)
				cancel()
				return
			}
		}
	}
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
	case "scratchpad_update", "leader_info":
		// Session-wide state changes worth waking the agent for — unless it
		// made the change itself and already knows.
		if env.From == b.agentID {
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

	slog.Info("watcher: relevant message detected, requesting check_messages signal",
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

	// Request a wake-up signal instead of sending synchronously: the signal
	// loop coalesces requests and retries failed sends, and this read loop
	// never blocks on a slow picobot socket.
	b.requestSignal("message")
}

// maybeWakeOnConnect decides whether a reconnect warrants waking the
// agent: peek the mailbox first. An unconditional wake-up would ask the
// agent to check messages only to find an empty mailbox (a "why was I
// woken?" turn). The peek is non-destructive — receive_messages still
// delivers everything. Peek failure fails OPEN (wake anyway): missing a
// real message is worse than an occasional empty wake-up.
func (b *Bridge) maybeWakeOnConnect() (bool, error) {
	pending, err := b.peekMailbox()
	if err != nil {
		slog.Warn("watcher: mailbox peek failed, waking agent anyway", "error", err)
		b.requestSignal("connected")
		return true, nil
	}
	if pending > 0 {
		b.requestSignal("connected")
		return true, nil
	}
	slog.Info("watcher: mailbox empty after reconnect, skipping connect wake-up")
	return false, nil
}

// requestSignal asks the signal loop to deliver a check_messages signal.
// Requests are coalesced: a capacity-1 channel with non-blocking send means
// any number of concurrent requests become at most one pending signal.
func (b *Bridge) requestSignal(reason string) {
	select {
	case b.signalCh <- struct{}{}:
	default:
	}
	slog.Debug("watcher: signal requested", "reason", reason)
}

// signalLoop delivers check_messages signals to the local picobot socket.
// A failed send is retried with capped exponential backoff until it succeeds,
// so a missed wake-up is never final (e.g. while picobot is restarting).
func (b *Bridge) signalLoop(ctx context.Context) {
	backoff := signalBaseBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.signalCh:
		}

		for {
			if ctx.Err() != nil {
				return
			}

			err := b.sendCheckMessagesSignal()
			if err == nil {
				backoff = signalBaseBackoff
				break
			}

			slog.Error("watcher: signal send failed, will retry", "error", err, "retry_in", backoff)
			if !sleepCtx(ctx, backoffJitter(backoff, signalMaxBackoff)) {
				return
			}
			backoff = min(backoff*2, signalMaxBackoff)
		}
	}
}

// sendCheckMessagesSignal sends the wake-up signal, attaching metadata about
// the most recent triggering message for auditing. The signal targets the
// chat session that most recently called a tool on this bridge (from
// tools/call _meta), so an agent that is a member of several chats is woken
// in the session the message belongs to — not a global default.
func (b *Bridge) sendCheckMessagesSignal() error {
	channel, chatID := b.originTarget()
	sig := signal.Signal{
		Source:  b.signalSource(),
		Action:  "check_messages",
		Channel: channel,
		ChatID:  chatID,
	}
	if info := pendingSignal.Load(); info != nil {
		sig.Metadata = map[string]interface{}{
			"trigger_type": info.TriggerType,
			"from_agent":   info.FromAgent,
			"sequence":     info.Sequence,
		}
	}
	_, err := signal.SendToSocket(b.signalSocketPath, sig)
	return err
}

// backoffJitter returns a duration in [d/2, d], capped at max.
func backoffJitter(d, max time.Duration) time.Duration {
	if d > max {
		d = max
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// getPendingSignalInfo returns and clears the pending signal metadata.
func getPendingSignalInfo() *pendingSignalInfo {
	return pendingSignal.Swap(nil)
}
