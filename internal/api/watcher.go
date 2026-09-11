package api

import (
	"log/slog"
	"sync"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

// Watcher allows SSE clients to subscribe to all message traffic in sessions.
type Watcher struct {
	mu   sync.RWMutex
	subs map[string]map[chan protocol.Envelope]struct{} // sessionID → set of channels
}

func NewWatcher() *Watcher {
	return &Watcher{
		subs: make(map[string]map[chan protocol.Envelope]struct{}),
	}
}

// Subscribe registers a channel to receive all envelopes for a session.
func (w *Watcher) Subscribe(sessionID string) chan protocol.Envelope {
	ch := make(chan protocol.Envelope, 128)
	w.mu.Lock()
	if w.subs[sessionID] == nil {
		w.subs[sessionID] = make(map[chan protocol.Envelope]struct{})
	}
	w.subs[sessionID][ch] = struct{}{}
	w.mu.Unlock()
	return ch
}

// Unsubscribe removes a channel. The channel is deliberately not closed:
// an unsubscribed channel simply becomes garbage once it is no longer
// referenced, which makes send-on-closed-channel races impossible.
func (w *Watcher) Unsubscribe(sessionID string, ch chan protocol.Envelope) {
	w.mu.Lock()
	if subs, ok := w.subs[sessionID]; ok {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(w.subs, sessionID)
		}
	}
	w.mu.Unlock()
}

// Notify sends an envelope to all subscribers of a session.
//
// The read lock is held for the entire loop so Unsubscribe cannot remove a
// channel mid-iteration. This is safe because sends are non-blocking — the
// lock is never held on a slow subscriber.
func (w *Watcher) Notify(sessionID string, env protocol.Envelope) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	for ch := range w.subs[sessionID] {
		select {
		case ch <- env:
		default:
			slog.Warn("watcher: subscriber channel full, dropping message", "session", sessionID)
		}
	}
}
