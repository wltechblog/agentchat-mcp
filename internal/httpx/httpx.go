// Package httpx builds HTTP clients that survive network changes.
//
// The default transport negotiates HTTP/2: all requests to a host
// multiplex over one TCP connection. When that connection dies silently
// (network change, wake-from-sleep), writes into it still succeed locally —
// the kernel buffers them — so every request waits for response headers
// until its timeout, and the transport keeps treating the corpse as usable
// forever. Field symptom: endless identical "context deadline exceeded
// (Client.Timeout exceeded while awaiting headers)" after switching
// networks, recovering only on restart.
package httpx

import (
	"net/http"
	"time"
)

const (
	// healthCheckSilence is slightly above the server's 5-minute SSE ping
	// interval, so an app-level ping keeps healthy connections "non-silent"
	// and the h2 health check never adds traffic during normal operation.
	healthCheckSilence = 5*time.Minute + 30*time.Second

	// healthCheckGrace bounds how long a health ping may go unanswered
	// before the connection is torn down (default would be 15s; pinned for
	// explicitness).
	healthCheckGrace = 15 * time.Second
)

// New returns an HTTP client whose transport health-checks HTTP/2
// connections: a connection silent for healthCheckSilence gets a ping
// frame, and one whose ping goes unanswered for healthCheckGrace is torn
// down — so the next request dials fresh on the current network instead of
// being written into a dead socket. A timeout of 0 means no client timeout
// (for long-lived streams; the transport health check still applies).
func New(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: healthCheckSilence,
		PingTimeout:     healthCheckGrace,
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}
