package httpx

import (
	"net/http"
	"testing"
	"time"
)

// TestNewConfiguresH2HealthChecks pins the dead-connection detection that
// makes clients recover across network changes (see package doc).
func TestNewConfiguresH2HealthChecks(t *testing.T) {
	c := New(30 * time.Second)
	if c.Timeout != 30*time.Second {
		t.Fatalf("timeout not applied: %v", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.Transport)
	}
	if tr.HTTP2 == nil {
		t.Fatal("HTTP2 health-check config missing")
	}
	if tr.HTTP2.SendPingTimeout != healthCheckSilence {
		t.Fatalf("SendPingTimeout = %v, want %v", tr.HTTP2.SendPingTimeout, healthCheckSilence)
	}
	if tr.HTTP2.PingTimeout != healthCheckGrace {
		t.Fatalf("PingTimeout = %v, want %v", tr.HTTP2.PingTimeout, healthCheckGrace)
	}

	// Must not mutate the shared default transport.
	dt := http.DefaultTransport.(*http.Transport)
	if dt.HTTP2 != nil && dt.HTTP2.SendPingTimeout == healthCheckSilence {
		t.Fatal("DefaultTransport was mutated")
	}

	// Zero timeout (long-lived streams) is preserved.
	if New(0).Timeout != 0 {
		t.Fatal("zero timeout should mean no client timeout")
	}
}
