package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/api"
	"github.com/wltechblog/agentchat-mcp/internal/filestore"
	"github.com/wltechblog/agentchat-mcp/internal/hub"
	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/persist"
	"github.com/wltechblog/agentchat-mcp/internal/presence"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

const (
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

	// File transfers stream to disk under filesRoot, so there is no RAM
	// bound on file size. AGENTCHAT_MAX_FILE_SIZE (bytes) optionally caps
	// it; unset or 0 means unlimited (the only real bound is disk space).
	maxFileSize := int64(0)
	if v := os.Getenv("AGENTCHAT_MAX_FILE_SIZE"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			slog.Warn("invalid AGENTCHAT_MAX_FILE_SIZE, files unlimited", "value", v, "error", err)
		} else {
			maxFileSize = n
		}
	}
	filesRoot := "agentchat-files"
	if dataDir := os.Getenv("AGENTCHAT_DATA"); dataDir != "" {
		filesRoot = filepath.Join(dataDir, "files")
	}
	fs := filestore.NewStore(filesRoot, maxFileSize)

	store := session.NewStore()
	sp := scratchpad.NewStore()
	pt := presence.NewTracker(presenceTTL)
	mb := mailbox.NewStore(mailboxMax)
	h := hub.New(hub.Deps{
		SessionStore: store,
		Scratchpad:   sp,
		Files:        fs,
		Presence:     pt,
		Mailboxes:    mb,
	}, hub.WithDebugLog(debugLog))

	// Channel janitor: reap sessions no agent has touched for the retention
	// horizon (default 30 days). AGENTCHAT_CHANNEL_RETENTION is a Go
	// duration ("720h", "30d" is not valid — use "720h", "2160h", ...);
	// "0" disables the janitor entirely.
	retention := 30 * 24 * time.Hour
	if v := os.Getenv("AGENTCHAT_CHANNEL_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			slog.Warn("invalid AGENTCHAT_CHANNEL_RETENTION, using 720h", "value", v, "error", err)
		} else {
			retention = d
		}
	}
	h.StartJanitor(retention, 6*time.Hour)

	// Persistence is opt-in via AGENTCHAT_DATA; without it the server runs
	// purely in memory, as before. Files live on disk under
	// <AGENTCHAT_DATA>/files but are NOT persisted across restarts — the
	// file store is wiped at startup (blobs belong to session state that
	// does not survive a restart either).
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

	slog.Info("starting server", "port", port, "persistent", pstore != nil)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Shutdown is coordinated here, not in a signal goroutine: main must not
	// exit before the final persistence flush has run.
	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		slog.Info("shutting down...", "signal", sig.String())
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
