package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

// payload helper is defined in hub_test.go.

// TestOfflineRecipientMessageStillDelivered pins the F2 delivery-contract fix:
// a known member that lapsed past the presence TTL (online=false) still
// receives direct messages — the mailbox is the delivery record, and
// onAgentWentOffline explicitly promises mail sent while offline is there on
// return. Previously SendMessage refused these sends (HTTP 400 "offline"),
// black-holing task_assigns/replies to mid-turn agents.
func TestOfflineRecipientMessageStillDelivered(t *testing.T) {
	h, store, _ := newTestHub(t, 50*time.Millisecond, 10*time.Millisecond)
	sess := store.Create("f2")

	h.Register(sess.ID, "agent-a", nil)
	if err := h.SendMessage(sess.ID, "agent-b", "agent-a", "message", payload("before")); err != nil {
		t.Fatalf("send to online agent: %v", err)
	}

	// Let agent-a lapse past the TTL; do NOT re-register it.
	time.Sleep(200 * time.Millisecond)

	agents := h.GetSessionAgents(sess.ID)
	if len(agents) != 1 || agents[0].Online {
		t.Fatalf("test setup: expected agent-a offline, got %+v", agents)
	}

	// The fix: offline-but-known recipients are deliverable.
	if err := h.SendMessage(sess.ID, "agent-b", "agent-a", "task_assign", payload("work")); err != nil {
		t.Fatalf("send to offline-but-known agent must be accepted, got: %v", err)
	}

	msgs := h.DrainMailbox(sess.ID, "agent-a")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 queued entries (1 pre-expiry + 1 post-expiry), got %d", len(msgs))
	}
	if msgs[1].Envelope.Type != "task_assign" || string(msgs[1].Envelope.Payload) != `{"text":"work"}` {
		t.Fatalf("post-expiry DM lost or wrong: %+v", msgs[1].Envelope)
	}

	// Returning to presence preserves identity (existing contract, unchanged).
	h.Register(sess.ID, "agent-a", nil)
	if agents := h.GetSessionAgents(sess.ID); len(agents) != 1 || !agents[0].Online {
		t.Fatalf("expected agent-a online again, got %+v", agents)
	}
}

// TestUnknownRecipientStillRejected guards the flip side: only genuinely
// unknown recipients (typo, wrong channel) get the actionable error.
func TestUnknownRecipientStillRejected(t *testing.T) {
	h, store, _ := newTestHub(t, 60*time.Second, 15*time.Second)
	sess := store.Create("f2-unknown")

	h.Register(sess.ID, "agent-a", nil)
	err := h.SendMessage(sess.ID, "agent-a", "no-such-agent", "message", payload("hi"))
	if err == nil {
		t.Fatal("send to unknown agent must fail")
	}
	if !strings.Contains(err.Error(), "no agent") || !strings.Contains(err.Error(), "list_agents") {
		t.Fatalf("expected unknown-recipient error, got %v", err)
	}
	// Nothing must have been recorded or delivered.
	if len(h.GetHistoryAfter(sess.ID, 0, 100)) != 0 {
		t.Fatal("rejected send must not enter history")
	}
	if n := len(h.DrainMailbox(sess.ID, "no-such-agent")); n != 0 {
		t.Fatalf("rejected send must not create a mailbox, got %d entries", n)
	}
}

// TestOfflineSendSurfacedInHistory verifies the observability half: the
// offline-queued delivery is a real recorded message (history + sequence),
// so the drain carries an ACK-able envelope — not a silent queue insert.
func TestOfflineSendSurfacedInHistory(t *testing.T) {
	h, store, _ := newTestHub(t, 50*time.Millisecond, 10*time.Millisecond)
	sess := store.Create("f2-hist")

	h.Register(sess.ID, "agent-a", nil)
	time.Sleep(200 * time.Millisecond) // agent-a offline

	if err := h.SendMessage(sess.ID, "agent-b", "agent-a", "task_status", payload("s")); err != nil {
		t.Fatalf("offline send: %v", err)
	}

	hist := h.GetHistoryAfter(sess.ID, 0, 100)
	found := false
	for _, e := range hist {
		if e.From == "agent-b" && e.To == "agent-a" && e.Type == "task_status" && e.Sequence > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("offline-delivered message missing from history (or unsequenced)")
	}
}

// TestBroadcastReportsFanOut checks Broadcast returns the recipient count
// and the HTTP-visible semantic stays "accepted, N mailboxes".
func TestBroadcastReportsFanOut(t *testing.T) {
	h, store, _ := newTestHub(t, 60*time.Second, 15*time.Second)
	sess := store.Create("f2-bc")

	h.Register(sess.ID, "agent-a", nil)
	h.Register(sess.ID, "agent-b", nil)

	n := h.Broadcast(sess.ID, "agent-a", "broadcast", payload("hi"))
	if n != 1 {
		t.Fatalf("expected 1 recipient (everyone except sender), got %d", n)
	}

	// Empty roster (fresh session, sender never registered): 0 must be
	// reported, not silently swallowed.
	sess2 := store.Create("f2-bc-empty")
	if n := h.Broadcast(sess2.ID, "nobody", "broadcast", payload("x")); n != 0 {
		t.Fatalf("expected 0 recipients on empty roster, got %d", n)
	}
}

// TestRegisterReannouncesReconnectedAgent pins the re-announce: a known
// agent whose presence lapsed and then heartbeats again gets an
// agent_reconnected event fanned out to peers (watchers + mailboxes),
// so the 30s/60s heartbeat-vs-TTL race no longer leaves stale "offline"
// views behind until the next list_agents.
func TestRegisterReannouncesReconnectedAgent(t *testing.T) {
	h, store, _ := newTestHub(t, 50*time.Millisecond, 10*time.Millisecond)
	sess := store.Create("f2-re")

	h.Register(sess.ID, "agent-a", []string{"write"})
	h.Register(sess.ID, "agent-b", nil)

	// agent-b's presence lapses; agent-a stays alive via RefreshPresence.
	time.Sleep(60 * time.Millisecond)
	h.RefreshPresence(sess.ID, "agent-a")

	agents := h.GetSessionAgents(sess.ID)
	for _, a := range agents {
		if a.AgentID == "agent-b" && a.Online {
			t.Fatal("test setup: expected agent-b offline before re-register")
		}
	}

	// Capture the real-time fan-out: ambient events go to watchers, never
	// into mailboxes (mirrors the agent_joined contract).
	var reannounces []protocol.Envelope
	h.SetNotifier(func(sid string, env protocol.Envelope) {
		if env.Type == protocol.TypeAgentReconnected {
			reannounces = append(reannounces, env)
		}
	})

	// agent-b's heartbeat lands.
	h.Register(sess.ID, "agent-b", nil)

	if len(reannounces) != 1 {
		t.Fatalf("expected exactly 1 agent_reconnected event, got %d", len(reannounces))
	}
	env := reannounces[0]
	if env.From != "server" || env.To != "*" || env.Sequence <= 0 {
		t.Fatalf("re-announce envelope malformed: %+v", env)
	}
	var info protocol.AgentInfo
	if err := json.Unmarshal(env.Payload, &info); err != nil {
		t.Fatalf("bad agent_reconnected payload: %v", err)
	}
	if info.AgentID != "agent-b" || !info.Online {
		t.Fatalf("re-announce wrong agent/state: %+v", info)
	}

	// Steady-state heartbeats (agent online the whole time) must NOT spam.
	reannounces = nil
	h.Register(sess.ID, "agent-a", nil)
	h.Register(sess.ID, "agent-a", nil)
	if len(reannounces) != 0 {
		t.Fatalf("steady-state heartbeat must not re-announce, got %d events", len(reannounces))
	}
}
