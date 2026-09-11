package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/api"
	"github.com/wltechblog/agentchat-mcp/internal/filestore"
	"github.com/wltechblog/agentchat-mcp/internal/hub"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/persist"
	"github.com/wltechblog/agentchat-mcp/internal/presence"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

const (
	maxFileSize = 50 << 20
	presenceTTL = 60 * time.Second
	mailboxMax  = 1000
	// stateFlushInterval is the worst-case loss window for persisted state.
	stateFlushInterval = 5 * time.Second
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	debugStr := os.Getenv("AGENTCHAT_DEBUG")
	debugLog := debugStr == "true" || debugStr == "1"

	store := session.NewStore()
	lt := leader.NewTracker()
	sp := scratchpad.NewStore()
	fs := filestore.NewStore(maxFileSize)
	pt := presence.NewTracker(presenceTTL)
	mb := mailbox.NewStore(mailboxMax)
	h := hub.New(hub.Deps{
		SessionStore: store,
		Leader:       lt,
		Scratchpad:   sp,
		Files:        fs,
		Presence:     pt,
		Mailboxes:    mb,
	}, hub.WithDebugLog(debugLog))

	// Persistence is opt-in via AGENTCHAT_DATA; without it the server runs
	// purely in memory, as before. Files are not persisted (they stay in
	// memory and are lost on restart).
	var pstore *persist.Store
	var flusher *persist.Flusher
	getSnapshot := func() persist.Snapshot {
		return persist.Snapshot{
			Sessions:   store.Snapshot(),
			Mailboxes:  mb.Snapshot(),
			History:    h.HistorySnapshot(),
			SeqNums:    h.SeqSnapshot(),
			Scratchpad: sp.Snapshot(),
		}
	}
	if dataDir := os.Getenv("AGENTCHAT_DATA"); dataDir != "" {
		ps, err := persist.New(dataDir)
		if err != nil {
			slog.Error("persist: disabled", "data_dir", dataDir, "error", err)
		} else {
			pstore = ps
			if snap, ok := ps.Load(); ok {
				restoreState(store, mb, h, sp, snap)
			} else {
				slog.Info("persist: no existing state, starting fresh", "data_dir", dataDir)
			}
			flusher = pstore.StartFlusher(getSnapshot, stateFlushInterval)
		}
	}

	handler := api.New(h, store)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down...")
		pt.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		if flusher != nil {
			// Final flush so a clean restart loses nothing.
			flusher.Stop()
			if err := pstore.Flush(getSnapshot()); err != nil {
				slog.Warn("persist: final flush failed", "error", err)
			}
		}
	}()

	slog.Info("starting server", "port", port, "persistent", pstore != nil)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

// restoreState reloads persisted state into the stores (startup only).
func restoreState(store *session.Store, mb *mailbox.Store, h *hub.Hub, sp *scratchpad.Store, snap persist.Snapshot) {
	store.Restore(snap.Sessions)
	mb.Restore(snap.Mailboxes)
	h.RestoreState(snap.History, snap.SeqNums)
	sp.Restore(snap.Scratchpad)
	slog.Info("persist: state restored",
		"sessions", len(snap.Sessions),
		"mailboxes", len(snap.Mailboxes),
		"history_sessions", len(snap.History),
	)
}
