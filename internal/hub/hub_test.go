package hub

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/filestore"
	"github.com/wltechblog/agentchat-mcp/internal/leader"
	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/persist"
	"github.com/wltechblog/agentchat-mcp/internal/presence"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/scratchpad"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

// newTestHub wires a Hub over fresh stores with a short presence TTL and
// sweep interval so expiry behavior can be tested quickly.
func newTestHub(t *testing.T, ttl, sweepInterval time.Duration) (*Hub, *session.Store, *presence.Tracker) {
	t.Helper()
	store := session.NewStore()
	pt := presence.NewTracker(ttl)
	h := New(Deps{
		SessionStore: store,
		Leader:       leader.NewTracker(),
		Scratchpad:   scratchpad.NewStore(),
		Files:        filestore.NewStore(1 << 20),
		Presence:     pt,
		Mailboxes:    mailbox.NewStore(1000),
	}, WithSweepInterval(sweepInterval))
	t.Cleanup(pt.Stop)
	return h, store, pt
}

// TestConcurrentSendNoRace hammers SendMessage/Broadcast/scratchpad writes
// from many goroutines while readers walk the history. Before the seqNums
// and history locking fixes this died with concurrent-map-write fatals and
// tripped the race detector in GetHistoryAfter.
func TestConcurrentSendNoRace(t *testing.T) {
	h, store, _ := newTestHub(t, 60*time.Second, 15*time.Second)
	sess := store.Create("race")

	for _, id := range []string{"agent-a", "agent-b", "agent-c"} {
		h.Register(sess.ID, id, nil)
	}

	payload, _ := json.Marshal(map[string]string{"x": "y"})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch n % 3 {
				case 0:
					_ = h.SendMessage(sess.ID, "agent-a", "agent-b", "message", payload)
				case 1:
					h.Broadcast(sess.ID, "agent-b", "broadcast", payload)
				case 2:
					_, _ = h.ScratchpadSet(sess.ID, "agent-c", "key", payload)
				}
			}
		}(i)
	}

	// Concurrent history readers: previously a data race with writers.
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = h.GetHistory(sess.ID)
					_ = h.GetHistoryAfter(sess.ID, 0, 50)
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	readerWG.Wait()

	// History order must match sequence order: strictly increasing sequences.
	history := h.GetHistory(sess.ID)
	for i := 1; i < len(history); i++ {
		if history[i].Sequence <= history[i-1].Sequence {
			t.Fatalf("history out of sequence order at %d: seq %d after %d",
				i, history[i].Sequence, history[i-1].Sequence)
		}
	}
}

// TestMailboxSurvivesPresenceExpiry verifies the core delivery guarantee:
// an agent that goes idle past the TTL still has its mailbox intact, still
// receives direct messages and broadcasts, and shows as online=false.
func TestMailboxSurvivesPresenceExpiry(t *testing.T) {
	h, store, _ := newTestHub(t, 50*time.Millisecond, 10*time.Millisecond)
	sess := store.Create("survive")

	h.Register(sess.ID, "agent-a", []string{"search"})
	if agents := h.GetSessionAgents(sess.ID); !agents[0].Online {
		t.Fatal("expected freshly registered agent to be online")
	}

	if err := h.SendMessage(sess.ID, "agent-b", "agent-a", "message", payload("before")); err != nil {
		t.Fatalf("send before expiry: %v", err)
	}

	// Let the agent lapse past the TTL and let the sweep run.
	time.Sleep(200 * time.Millisecond)

	agents := h.GetSessionAgents(sess.ID)
	if len(agents) != 1 {
		t.Fatalf("expected expired agent to stay listed, got %d agents", len(agents))
	}
	if agents[0].Online {
		t.Fatal("expected expired agent to show online=false")
	}
	if len(agents[0].Capabilities) != 1 || agents[0].Capabilities[0] != "search" {
		t.Fatalf("expected capabilities to survive expiry, got %v", agents[0].Capabilities)
	}

	// Sends to the offline agent must succeed, not fail.
	if err := h.SendMessage(sess.ID, "agent-b", "agent-a", "message", payload("after")); err != nil {
		t.Fatalf("send to expired agent should succeed: %v", err)
	}
	h.Broadcast(sess.ID, "agent-b", "broadcast", payload("bcast"))

	msgs := h.DrainMailbox(sess.ID, "agent-a")
	// DM before expiry, DM + broadcast after expiry. Going offline announces
	// nothing: agent_left fires only on true departure (the forget horizon).
	if len(msgs) != 3 {
		t.Fatalf("expected 3 queued entries, got %d", len(msgs))
	}
	gotTypes := map[string]bool{}
	for _, m := range msgs {
		gotTypes[m.Envelope.Type] = true
	}
	for _, want := range []string{"message", "broadcast"} {
		if !gotTypes[want] {
			t.Fatalf("expected %q in drained mailbox, got %v", want, gotTypes)
		}
	}
	if gotTypes["agent_left"] {
		t.Fatal("agent_left must never enter mailboxes")
	}

	// Coming back online preserves identity and capabilities.
	h.Register(sess.ID, "agent-a", nil)
	agents = h.GetSessionAgents(sess.ID)
	if len(agents) != 1 || !agents[0].Online {
		t.Fatalf("expected returning agent online, got %+v", agents)
	}
	if len(agents[0].Capabilities) != 1 || agents[0].Capabilities[0] != "search" {
		t.Fatalf("expected capabilities to survive the round trip, got %v", agents[0].Capabilities)
	}
}

// TestLeaderClearsWhenNoOnlineAgents: when the leader expires and nobody is
// online, leadership must be released so the next agent to join leads.
func TestLeaderClearsWhenNoOnlineAgents(t *testing.T) {
	h, store, _ := newTestHub(t, 50*time.Millisecond, 10*time.Millisecond)
	sess := store.Create("leader-clear")

	h.Register(sess.ID, "agent-a", nil)
	if h.GetLeader(sess.ID) != "agent-a" {
		t.Fatal("expected agent-a to lead")
	}

	time.Sleep(200 * time.Millisecond) // expire; no one else online

	if got := h.GetLeader(sess.ID); got != "" {
		t.Fatalf("expected leadership cleared after last agent expired, got %q", got)
	}

	// A new joiner becomes leader instead of inheriting a ghost.
	h.Register(sess.ID, "agent-b", nil)
	if got := h.GetLeader(sess.ID); got != "agent-b" {
		t.Fatalf("expected new joiner to lead, got %q", got)
	}
}

// TestLeaderTransfersToOnlineAgentOnExpiry: when the leader expires, an
// online agent takes over — not another offline one.
func TestLeaderTransfersToOnlineAgentOnExpiry(t *testing.T) {
	h, store, _ := newTestHub(t, 50*time.Millisecond, 10*time.Millisecond)
	sess := store.Create("leader-transfer")

	h.Register(sess.ID, "agent-a", nil)
	h.Register(sess.ID, "agent-b", nil)
	h.Register(sess.ID, "agent-c", nil)

	// Only agent-c stays online past the TTL (simulated by continued activity).
	deadline := time.Now().Add(2 * time.Second)
	for h.GetLeader(sess.ID) != "agent-c" {
		h.RefreshPresence(sess.ID, "agent-c")
		if time.Now().After(deadline) {
			t.Fatalf("expected leadership to land on agent-c, got %q", h.GetLeader(sess.ID))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func payload(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"text": text})
	return b
}

// TestAllEventsSequencedAndFannedOut: every event — joins, messages,
// broadcasts, scratchpad, leader — must carry a sequence from the session's
// single counter and reach the SSE fan-out exactly once. Before the pipeline
// unification, scratchpad/leader/left events never reached watchers at all.
func TestAllEventsSequencedAndFannedOut(t *testing.T) {
	h, store, _ := newTestHub(t, 60*time.Second, 15*time.Second)
	sess := store.Create("fanout")

	var mu sync.Mutex
	var sse []protocol.Envelope
	h.SetNotifier(func(sessionID string, env protocol.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		if sessionID != sess.ID {
			t.Errorf("notifier got wrong session %q", sessionID)
		}
		sse = append(sse, env)
	})

	h.Register(sess.ID, "agent-a", nil)
	h.Register(sess.ID, "agent-b", nil)
	if err := h.SendMessage(sess.ID, "agent-a", "agent-b", "message", payload("hi")); err != nil {
		t.Fatalf("send: %v", err)
	}
	h.Broadcast(sess.ID, "agent-a", "broadcast", payload("bc"))
	if _, err := h.ScratchpadSet(sess.ID, "agent-a", "plan", payload("v")); err != nil {
		t.Fatalf("scratchpad set: %v", err)
	}
	if err := h.LeaderTransfer(sess.ID, "agent-a", "agent-b"); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if err := h.ScratchpadDelete(sess.ID, "agent-b", "plan"); err != nil {
		t.Fatalf("scratchpad delete: %v", err)
	}
	h.Register(sess.ID, "agent-c", nil)

	mu.Lock()
	defer mu.Unlock()

	wantTypes := []string{
		"agent_joined", "agent_joined", "message", "broadcast",
		"scratchpad_update", "leader_info", "scratchpad_update", "agent_joined",
	}
	if len(sse) != len(wantTypes) {
		got := make([]string, len(sse))
		for i, e := range sse {
			got[i] = e.Type
		}
		t.Fatalf("expected %v on the watcher fan-out, got %v", wantTypes, got)
	}
	last := int64(0)
	for i, env := range sse {
		if env.Type != wantTypes[i] {
			t.Fatalf("event %d: expected %q, got %q", i, wantTypes[i], env.Type)
		}
		if env.Sequence != last+1 {
			t.Fatalf("event %d (%s): sequences must advance by one across ALL event types, got %d after %d",
				i, env.Type, env.Sequence, last)
		}
		last = env.Sequence
	}

	// Mailbox-side spot check: agent-b should hold the direct message, the
	// broadcast, agent-a's scratchpad update, and the leader info. Joins are
	// ambient (watch stream only) and b's own scratchpad delete excludes b.
	msgs := h.DrainMailbox(sess.ID, "agent-b")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 mailbox entries for agent-b, got %d", len(msgs))
	}
	for _, m := range msgs {
		if m.Envelope.Sequence == 0 {
			t.Fatalf("mailbox entry of type %s has no sequence", m.Envelope.Type)
		}
	}
}

// TestPersistenceRestartRoundTrip simulates a server restart: capture a
// snapshot from live stores, restore it into fresh ones, and verify that
// sessions, queued mail, history, sequence continuity, and the scratchpad
// all survive. This is what makes a restart a bump instead of a lobotomy.
func TestPersistenceRestartRoundTrip(t *testing.T) {
	// Live world.
	store := session.NewStore()
	pt := presence.NewTracker(time.Minute)
	defer pt.Stop()
	mb := mailbox.NewStore(100)
	sp := scratchpad.NewStore()
	h := New(Deps{SessionStore: store, Leader: leader.NewTracker(), Scratchpad: sp,
		Files: filestore.NewStore(1 << 20), Presence: pt, Mailboxes: mb}, WithSweepInterval(time.Hour))

	sess := store.Create("persist")
	h.Register(sess.ID, "agent-a", []string{"search"})
	if err := h.SendMessage(sess.ID, "agent-a", "agent-b", "message", payload("queued")); err != nil {
		t.Fatalf("send: %v", err)
	}
	sp.Set(sess.ID, "plan", payload("step 1"), "agent-a")

	// Snapshot, then a fresh "process".
	snap := persist.Snapshot{
		Sessions:   store.Snapshot(),
		Mailboxes:  mb.Snapshot(),
		History:    h.HistorySnapshot(),
		SeqNums:    h.SeqSnapshot(),
		Scratchpad: sp.Snapshot(),
	}

	store2 := session.NewStore()
	pt2 := presence.NewTracker(time.Minute)
	defer pt2.Stop()
	mb2 := mailbox.NewStore(100)
	sp2 := scratchpad.NewStore()
	h2 := New(Deps{SessionStore: store2, Leader: leader.NewTracker(), Scratchpad: sp2,
		Files: filestore.NewStore(1 << 20), Presence: pt2, Mailboxes: mb2}, WithSweepInterval(time.Hour))
	store2.Restore(snap.Sessions)
	mb2.Restore(snap.Mailboxes)
	h2.RestoreState(snap.History, snap.SeqNums)
	sp2.Restore(snap.Scratchpad)

	// Session credentials survived.
	if _, ok := store2.ValidatePSK(sess.ID, sess.PSK); !ok {
		t.Fatal("session lost across restart")
	}

	// Queued mail survived.
	msgs := mb2.Drain(sess.ID + "/agent-b")
	if len(msgs) != 1 || msgs[0].Envelope.Payload == nil {
		t.Fatalf("queued mail lost across restart: %+v", msgs)
	}

	// Sequence continuity: the next message must not reuse old numbers.
	h2.Register(sess.ID, "agent-a", nil)
	if err := h2.SendMessage(sess.ID, "agent-a", "agent-b", "message", payload("after")); err != nil {
		t.Fatalf("send after restore: %v", err)
	}
	hist := h2.GetHistory(sess.ID)
	if len(hist) != 2 {
		t.Fatalf("expected restored history + new message, got %d entries", len(hist))
	}
	if hist[1].Sequence <= hist[0].Sequence {
		t.Fatalf("sequence restarted after restore: %d then %d", hist[0].Sequence, hist[1].Sequence)
	}

	// Scratchpad survived.
	if _, ok := sp2.Get(sess.ID, "plan"); !ok {
		t.Fatal("scratchpad lost across restart")
	}
}

// TestAgentLeftOnlyOnTrueDeparture: a TTL lapse is silent — no event, mailbox
// kept. agent_left is announced (on the watch stream only) when the agent is
// forgotten entirely, and that is also when the mailbox is reclaimed.
func TestAgentLeftOnlyOnTrueDeparture(t *testing.T) {
	var mu sync.Mutex
	var sse []protocol.Envelope
	h, store, pt := newTestHub(t, 40*time.Millisecond, 5*time.Millisecond)
	h.SetNotifier(func(sessionID string, env protocol.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		sse = append(sse, env)
	})
	pt.SetForgetAfter(80 * time.Millisecond)
	sess := store.Create("depart")
	h.Register(sess.ID, "agent-a", nil)
	if err := h.SendMessage(sess.ID, "peer", "agent-a", "message", payload("held")); err != nil {
		t.Fatalf("send: %v", err)
	}

	countLeft := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, env := range sse {
			if env.Type == "agent_left" {
				n++
			}
		}
		return n
	}

	// Past the TTL (40ms) but before the forget horizon (80ms): offline, but
	// no departure announcement.
	time.Sleep(60 * time.Millisecond)
	if n := countLeft(); n != 0 {
		t.Fatalf("TTL lapse must not announce agent_left, got %d", n)
	}

	// Past the forget horizon: exactly one agent_left, and the mailbox is gone.
	time.Sleep(200 * time.Millisecond)
	if n := countLeft(); n != 1 {
		t.Fatalf("expected exactly one agent_left at forget, got %d", n)
	}
	if msgs := h.DrainMailbox(sess.ID, "agent-a"); len(msgs) != 0 {
		t.Fatalf("expected mailbox reclaimed at forget, got %d entries", len(msgs))
	}
}
