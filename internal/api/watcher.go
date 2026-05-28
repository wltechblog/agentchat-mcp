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

// Unsubscribe removes a channel.
func (w *Watcher) Unsubscribe(sessionID string, ch chan protocol.Envelope) {
	w.mu.Lock()
	if subs, ok := w.subs[sessionID]; ok {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(w.subs, sessionID)
		}
		close(ch)
	}
	w.mu.Unlock()
}

// Notify sends an envelope to all subscribers of a session.
func (w *Watcher) Notify(sessionID string, env protocol.Envelope) {
	w.mu.RLock()
	subs := w.subs[sessionID]
	w.mu.RUnlock()

	for ch := range subs {
		select {
		case ch <- env:
		default:
			slog.Warn("watcher: subscriber channel full, dropping message", "session", sessionID)
		}
	}
}
