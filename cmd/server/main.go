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
	"github.com/wltechblog/agentchat-mcp/internal/presence"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

const (
	maxFileSize   = 50 << 20
	presenceTTL   = 60 * time.Second
	mailboxMax    = 1000
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
	h := hub.New(store, lt, sp, fs, pt, mb, hub.WithDebugLog(debugLog))
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
	}()

	slog.Info("starting server", "port", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
