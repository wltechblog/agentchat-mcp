// Package persist provides opt-in crash-safe persistence of server state as
// a JSON snapshot with atomic replacement. The data set is deliberately
// bounded (history is capped per session, mailboxes per agent), so a full
// snapshot write stays small.
package persist

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/mailbox"
	"github.com/wltechblog/agentchat-mcp/internal/protocol"
	"github.com/wltechblog/agentchat-mcp/internal/session"
)

// Snapshot is the full durable server state.
//
// Deliberately absent: presence (ephemeral — agents re-heartbeat, and since
// presence no longer gates delivery, offline agents simply resume offline),
// watch tokens (re-issued on the next register), the file store (still
// in-memory; blobs are large), and the leader map (the first agent to
// re-heartbeat after a restart becomes leader — restoring a leader with no
// presence record would create a ghost that can never expire).
type Snapshot struct {
	Sessions   []session.Session                     `json:"sessions"`
	Mailboxes  map[string][]mailbox.Entry            `json:"mailboxes"`
	History    map[string][]protocol.Envelope        `json:"history"`
	SeqNums    map[string]int64                      `json:"seq_nums"`
	Scratchpad map[string][]protocol.ScratchpadEntry `json:"scratchpad"`
}

const stateFile = "state.json"

// Store persists snapshots under a data directory.
type Store struct {
	dir string
}

// New creates the data directory if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, stateFile) }

// Flush writes the snapshot atomically: write to a temp file, fsync, rename
// over the previous state. A crash mid-write leaves the old state intact.
func (s *Store) Flush(snap Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}

	f, err := os.CreateTemp(s.dir, "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close snapshot: %w", err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace snapshot: %w", err)
	}
	return nil
}

// Load returns the persisted snapshot, or ok=false when none exists. A
// corrupt snapshot is logged and ignored — an empty server beats a crash
// loop.
func (s *Store) Load() (Snapshot, bool) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		return Snapshot{}, false
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		slog.Error("persist: corrupt state file ignored", "path", s.path(), "error", err)
		return Snapshot{}, false
	}
	return snap, true
}

// Flusher periodically persists snapshots until stopped.
type Flusher struct {
	stopCh  chan struct{}
	stopped sync.Once
}

// StartFlusher persists getSnapshot() on a fixed interval until Stop. The
// interval is the worst-case loss window: a crash loses at most that much
// mail.
func (s *Store) StartFlusher(getSnapshot func() Snapshot, interval time.Duration) *Flusher {
	f := &Flusher{stopCh: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-f.stopCh:
				return
			case <-ticker.C:
				if err := s.Flush(getSnapshot()); err != nil {
					slog.Warn("persist: flush failed", "error", err)
				}
			}
		}
	}()
	return f
}

// Stop halts the flusher. Safe to call multiple times.
func (f *Flusher) Stop() {
	f.stopped.Do(func() { close(f.stopCh) })
}
