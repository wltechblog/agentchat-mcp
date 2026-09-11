package persist

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

func testSnapshot() Snapshot {
	return Snapshot{
		Sessions: []session.Session{
			{ID: "abc", Name: "test", PSK: "psk", CreatedAt: time.Now().UTC()},
		},
		Mailboxes: map[string][]mailbox.Entry{
			"abc/agent-b": {
				{ID: "m-1", Envelope: protocol.Envelope{Type: "message", From: "agent-a", To: "agent-b", Sequence: 1}, Received: time.Now().UTC()},
			},
		},
		History: map[string][]protocol.Envelope{
			"abc": {{Type: "message", From: "agent-a", To: "agent-b", Sequence: 1}},
		},
		SeqNums:    map[string]int64{"abc": 1},
		Scratchpad: map[string][]protocol.ScratchpadEntry{},
	}
}

func TestFlushLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := testSnapshot()
	if err := s.Flush(want); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, ok := s.Load()
	if !ok {
		t.Fatal("expected snapshot to load")
	}
	if len(got.Sessions) != 1 || got.Sessions[0].ID != "abc" {
		t.Fatalf("sessions not round-tripped: %+v", got.Sessions)
	}
	if entries := got.Mailboxes["abc/agent-b"]; len(entries) != 1 || entries[0].Envelope.From != "agent-a" {
		t.Fatalf("mailboxes not round-tripped: %+v", got.Mailboxes)
	}
	if got.SeqNums["abc"] != 1 {
		t.Fatalf("sequence counters not round-tripped: %+v", got.SeqNums)
	}

	// Temp files must not accumulate.
	matches, _ := filepath.Glob(filepath.Join(dir, "state-*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestLoadMissingFile(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := s.Load(); ok {
		t.Fatal("expected no snapshot for empty dir")
	}
}

func TestLoadCorruptIgnored(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := s.Load(); ok {
		t.Fatal("corrupt snapshot must be ignored, not fatal")
	}
}

func TestFlusherStops(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f := s.StartFlusher(testSnapshot, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	f.Stop()

	if _, ok := s.Load(); !ok {
		t.Fatal("flusher never wrote a snapshot")
	}
}
