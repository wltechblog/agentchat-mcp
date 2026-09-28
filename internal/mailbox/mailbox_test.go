package mailbox

import (
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

func env(from, msgType string, seq int64) protocol.Envelope {
	return protocol.Envelope{Type: msgType, From: from, To: "target", Sequence: seq}
}

func TestDrainMatchingKeepsNonMatching(t *testing.T) {
	s := NewStore(100)
	s.Deliver("box", env("agent-a", "message", 1))
	s.Deliver("box", env("agent-b", "message", 2))
	s.Deliver("box", env("agent-a", "task_result", 3))

	matched := s.DrainMatching("box", "agent-a", "")
	if len(matched) != 2 {
		t.Fatalf("expected 2 matches for agent-a, got %d", len(matched))
	}
	for _, e := range matched {
		if e.Envelope.From != "agent-a" {
			t.Fatalf("non-matching entry drained: %+v", e.Envelope)
		}
	}

	rest := s.Drain("box")
	if len(rest) != 1 || rest[0].Envelope.From != "agent-b" {
		t.Fatalf("expected agent-b's message to stay queued, got %+v", rest)
	}
}

func TestDrainMatchingWaitImmediateMatch(t *testing.T) {
	s := NewStore(100)
	s.Deliver("box", env("agent-a", "message", 1))

	start := time.Now()
	entries := s.DrainMatchingWait("box", "agent-a", "", 5*time.Second)
	if len(entries) != 1 {
		t.Fatalf("expected immediate match, got %d", len(entries))
	}
	if time.Since(start) > time.Second {
		t.Fatalf("existing match should return immediately, took %v", time.Since(start))
	}
}

func TestDrainMatchingWaitWakesOnDeliver(t *testing.T) {
	s := NewStore(100)

	go func() {
		time.Sleep(150 * time.Millisecond)
		s.Deliver("box", env("agent-a", "message", 1))
	}()

	start := time.Now()
	entries := s.DrainMatchingWait("box", "agent-a", "", 10*time.Second)
	elapsed := time.Since(start)

	if len(entries) != 1 {
		t.Fatalf("expected waiter to wake with the delivered message, got %d", len(entries))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("deliver did not wake the waiter, took %v", elapsed)
	}
}

func TestDrainMatchingWaitTimesOut(t *testing.T) {
	s := NewStore(100)

	start := time.Now()
	entries := s.DrainMatchingWait("box", "nobody", "", 200*time.Millisecond)
	elapsed := time.Since(start)

	if len(entries) != 0 {
		t.Fatalf("expected timeout with no entries, got %d", len(entries))
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("timeout wait took %v, expected ~200ms", elapsed)
	}
}

func TestDeleteSessionRemovesOnlyItsBoxes(t *testing.T) {
	s := NewStore(100)
	s.Deliver("s1/a", env("x", "message", 1))
	s.Deliver("s1/b", env("x", "message", 2))
	s.Deliver("s2/a", env("x", "message", 3))

	s.DeleteSession("s1")

	if got := s.Drain("s1/a"); len(got) != 0 {
		t.Fatalf("expected s1/a reaped, got %d entries", len(got))
	}
	if got := s.Drain("s1/b"); len(got) != 0 {
		t.Fatalf("expected s1/b reaped, got %d entries", len(got))
	}
	if got := s.Drain("s2/a"); len(got) != 1 {
		t.Fatalf("expected s2/a untouched, got %d entries", len(got))
	}
}
