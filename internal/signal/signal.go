package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// Signal represents an external trigger to send to a picobot instance.
// Signals are action-based — they carry a named action, not freeform instructions.
type Signal struct {
	// Source identifies the system sending the signal (e.g., "agentchat-mcp").
	Source string `json:"source"`

	// Action is the registered action name (e.g., "check_messages").
	Action string `json:"action"`

	// Timestamp is Unix millis when the signal was sent.
	Timestamp int64 `json:"timestamp,omitempty"`

	// Channel is the chat channel to inject the message into.
	// If empty, "signal" is used.
	Channel string `json:"channel,omitempty"`

	// ChatID is the specific conversation to target.
	// If empty, "default" is used.
	ChatID string `json:"chat_id,omitempty"`

	// Metadata holds optional structured data for logging/auditing only.
	// NEVER exposed to the agent or used in response text.
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// SendToSocket sends a signal to a picobot Unix domain socket and returns the response.
func SendToSocket(socketPath string, sig Signal) (map[string]string, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Set timestamp if not provided
	if sig.Timestamp == 0 {
		sig.Timestamp = time.Now().UnixMilli()
	}

	data, err := json.Marshal(sig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal signal: %w", err)
	}

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("failed to write: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return map[string]string{"status": "sent"}, nil
		}
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var resp map[string]string
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return map[string]string{"status": "sent", "raw": string(buf[:n])}, nil
	}
	return resp, nil
}

// WaitForSocket repeatedly tries to connect to the socket until it succeeds or context is cancelled.
func WaitForSocket(ctx context.Context, socketPath string, interval time.Duration) error {
	if interval == 0 {
		interval = 2 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(interval)
	}
}

// ReadAll reads all data from a connection with a deadline.
func ReadAll(conn net.Conn) ([]byte, error) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var result []byte
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			result = append(result, buf[:n])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				break
			}
			return result, err
		}
		if n == 0 {
			break
		}
	}
	return result, nil
}
