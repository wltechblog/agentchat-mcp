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
type Signal struct {
	Type     string                 `json:"type"`
	Channel  string                 `json:"channel,omitempty"`
	ChatID   string                 `json:"chat_id,omitempty"`
	Content  string                 `json:"content"`
	Priority string                 `json:"priority,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// SendToSocket sends a signal to a picobot Unix domain socket and returns the response.
func SendToSocket(socketPath string, sig Signal) (map[string]string, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", socketPath, err)
	}
	defer conn.Close()

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
			// Timeout is acceptable — server may not respond
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
// Useful for waiting until picobot is ready.
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
			result = append(result, buf[:n]...)
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
