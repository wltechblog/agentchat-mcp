package session

import (
	"testing"
	"time"
)

func TestCreateInitializesLastActive(t *testing.T) {
	s := NewStore()
	sess := s.Create("ch")
	if !sess.LastActive.Equal(sess.CreatedAt) {
		t.Fatalf("expected LastActive seeded from CreatedAt, got %v vs %v", sess.LastActive, sess.CreatedAt)
	}
}

func TestTouchActivityMonotonic(t *testing.T) {
	s := NewStore()
	sess := s.Create("ch")

	later := sess.LastActive.Add(time.Hour)
	if !s.TouchActivity(sess.ID, later) {
		t.Fatal("expected touch on existing session")
	}
	got, _ := s.Get(sess.ID)
	if !got.LastActive.Equal(later) {
		t.Fatalf("expected LastActive advanced to %v, got %v", later, got.LastActive)
	}

	// An older timestamp never regresses it.
	s.TouchActivity(sess.ID, sess.CreatedAt)
	got, _ = s.Get(sess.ID)
	if !got.LastActive.Equal(later) {
		t.Fatalf("LastActive regressed: %v", got.LastActive)
	}

	// Unknown sessions report false.
	if s.TouchActivity("nope", time.Now()) {
		t.Fatal("expected false for unknown session")
	}
}
