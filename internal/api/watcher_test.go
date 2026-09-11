package api

import (
	"testing"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

// TestNotifyDuringUnsubscribe hammers Notify while a subscriber disconnects.
// Before the fix (Notify iterating outside the lock + Unsubscribe closing
// channels) this tripped the race detector and panicked with
// send-on-closed-channel.
func TestNotifyDuringUnsubscribe(t *testing.T) {
	w := NewWatcher()
	leaving := w.Subscribe("s")
	staying := w.Subscribe("s")

	stop := make(chan struct{})
	notifyDone := make(chan struct{})
	go func() {
		defer close(notifyDone)
		for {
			select {
			case <-stop:
				return
			default:
				w.Notify("s", protocol.Envelope{Type: "message"})
			}
		}
	}()

	time.Sleep(2 * time.Millisecond)
	w.Unsubscribe("s", leaving)
	time.Sleep(2 * time.Millisecond)
	close(stop)
	<-notifyDone

	// The unsubscribed channel must never be closed by Unsubscribe.
	select {
	case _, ok := <-leaving:
		if !ok {
			t.Fatal("Unsubscribe must not close the subscriber channel")
		}
	default:
		// no buffered values — also fine
	}

	// Remaining subscribers still receive events after the unsubscribe.
	// Drain buffered values from the hammering phase first.
drain:
	for {
		select {
		case <-staying:
		default:
			break drain
		}
	}
	w.Notify("s", protocol.Envelope{Type: "broadcast"})
	select {
	case env := <-staying:
		if env.Type != "broadcast" {
			t.Fatalf("expected broadcast, got %q", env.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("staying subscriber did not receive the event")
	}
}

func TestNotifyDropsForSlowSubscriberWithoutBlocking(t *testing.T) {
	w := NewWatcher()
	slow := w.Subscribe("s") // nobody reads from slow
	fast := w.Subscribe("s")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			w.Notify("s", protocol.Envelope{Type: "message"})
		}
	}()

	select {
	case <-done:
		// Notify must return promptly despite the full slow subscriber.
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a full subscriber channel")
	}

	w.Unsubscribe("s", slow)
	w.Unsubscribe("s", fast)
}
