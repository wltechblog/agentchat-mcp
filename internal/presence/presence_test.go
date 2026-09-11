package presence

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestNewTrackerHonorsTTL(t *testing.T) {
	tr := NewTracker(30 * time.Millisecond)
	tr.Touch("s", "a", nil)

	if !tr.IsPresent("s", "a") {
		t.Fatal("expected agent present right after touch")
	}
	time.Sleep(40 * time.Millisecond)
	if tr.IsPresent("s", "a") {
		t.Fatal("expected agent absent after custom TTL elapsed")
	}
}

func TestDefaultTTLWhenNonPositive(t *testing.T) {
	tr := NewTracker(0)
	if tr.ttl != defaultTTL {
		t.Fatalf("expected default TTL for zero arg, got %v", tr.ttl)
	}
	tr = NewTracker(-5 * time.Minute)
	if tr.ttl != defaultTTL {
		t.Fatalf("expected default TTL for negative arg, got %v", tr.ttl)
	}
}

func TestSweepFiresOnceAndKeepsState(t *testing.T) {
	tr := NewTracker(30 * time.Millisecond)
	var expireCalls atomic.Int32
	tr.StartSweep(5*time.Millisecond, func(sessionID, agentID string, capabilities []string) {
		expireCalls.Add(1)
	}, func(sessionID, agentID string) {
		t.Errorf("unexpected forget for %s", agentID)
	})
	defer tr.Stop()

	tr.Touch("s", "a", []string{"search"})
	time.Sleep(100 * time.Millisecond) // several sweeps past the TTL

	if got := expireCalls.Load(); got != 1 {
		t.Fatalf("expiry callback should fire exactly once per lapse, got %d", got)
	}

	// The agent stays listed as offline with capabilities intact.
	agents := tr.GetAgents("s")
	if len(agents) != 1 {
		t.Fatalf("expected expired agent to remain listed, got %d", len(agents))
	}
	if agents[0].Online {
		t.Fatal("expected online=false for expired agent")
	}
	if len(agents[0].Capabilities) != 1 || agents[0].Capabilities[0] != "search" {
		t.Fatalf("expected capabilities preserved, got %v", agents[0].Capabilities)
	}
}

func TestTouchRevivesExpiredAgent(t *testing.T) {
	tr := NewTracker(30 * time.Millisecond)
	tr.Touch("s", "a", []string{"search"})
	time.Sleep(40 * time.Millisecond) // lapse past TTL

	// A nil-capabilities touch (e.g. presence refresh on a plain request)
	// must bring the agent back online without wiping its capabilities.
	tr.Touch("s", "a", nil)

	agents := tr.GetAgents("s")
	if len(agents) != 1 || !agents[0].Online {
		t.Fatalf("expected revived agent online, got %+v", agents)
	}
	if len(agents[0].Capabilities) != 1 || agents[0].Capabilities[0] != "search" {
		t.Fatalf("expected capabilities preserved on revive, got %v", agents[0].Capabilities)
	}
}

func TestForgetRemovesState(t *testing.T) {
	tr := NewTracker(10 * time.Millisecond)
	tr.forgetAfter = 30 * time.Millisecond
	var forgetCalls atomic.Int32
	tr.StartSweep(5*time.Millisecond, func(sessionID, agentID string, capabilities []string) {
		// expiry fires first; nothing to assert here
	}, func(sessionID, agentID string) {
		forgetCalls.Add(1)
	})
	defer tr.Stop()

	tr.Touch("s", "a", nil)
	time.Sleep(100 * time.Millisecond)

	if got := forgetCalls.Load(); got != 1 {
		t.Fatalf("forget should fire exactly once, got %d", got)
	}
	if agents := tr.GetAgents("s"); len(agents) != 0 {
		t.Fatalf("expected forgotten agent removed, got %d", len(agents))
	}
}
