package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEParser(t *testing.T) {
	var p sseParser
	lines := []string{
		"retry: 3000", // advisory field: consumed, no frame
		"",
		": ping", // keepalive comment
		"",
		"event: connected",
		`data: {"session":"s1"}`,
		"",
		"event: message",
		`data: {"a":`,
		`data: 2}`, // multi-line data joins with \n per the SSE spec
		"",
		"event: message\r",
		"data: {\"crlf\":true}\r", // CRLF line endings
		"data:no-space",           // space after colon is optional
		"",
	}

	var frames []sseFrame
	for _, l := range lines {
		if f, ok := p.feed(l); ok {
			frames = append(frames, f)
		}
	}

	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d: %+v", len(frames), frames)
	}
	if frames[0].Event != "connected" || frames[0].Data != `{"session":"s1"}` {
		t.Fatalf("frame 0 wrong: %+v", frames[0])
	}
	if frames[1].Event != "message" || frames[1].Data != "{\"a\":\n2}" {
		t.Fatalf("frame 1 wrong: %+v", frames[1])
	}
	if frames[2].Event != "message" || frames[2].Data != "{\"crlf\":true}\nno-space" {
		t.Fatalf("frame 2 wrong: %q / %q", frames[2].Event, frames[2].Data)
	}
}

// TestWatchdogForcesReconnectOnDeadStream: after a network change or
// wake-from-sleep the old TCP connection dies silently and the SSE read
// blocks forever — the watchdog must abandon it so watchLoop reconnects.
// Before this fix the CLI hung until the user exited and restarted.
func TestWatchdogForcesReconnectOnDeadStream(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/watch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "event: connected\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // then go silent forever
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &CLI{serverURL: srv.URL, sessionID: "s", psk: "p", client: &http.Client{Timeout: 5 * time.Second}, WatchdogTimeout: 150 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- c.watchSSE(make(chan struct{})) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "forcing reconnect") {
			t.Fatalf("expected watchdog reconnect error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watchSSE hung on a dead stream — watchdog did not fire")
	}
}

// TestWatchdogToleratesHealthyStream: a stream with keepalive traffic must
// never trip the watchdog, and quit must still end it promptly.
func TestWatchdogToleratesHealthyStream(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/watch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fl.Flush()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &CLI{serverURL: srv.URL, sessionID: "s", psk: "p", client: &http.Client{Timeout: 5 * time.Second}, WatchdogTimeout: 250 * time.Millisecond}
	quit := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- c.watchSSE(quit) }()

	select {
	case err := <-done:
		t.Fatalf("healthy stream must stay up, got %v", err)
	case <-time.After(700 * time.Millisecond):
		// 14 keepalives, zero watchdog trips — good.
	}

	close(quit)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("quit should end the stream cleanly, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchSSE did not exit on quit")
	}
}
