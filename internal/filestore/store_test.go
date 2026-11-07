package filestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreComputesSha256(t *testing.T) {
	s := NewStore(t.TempDir(), 1<<20)
	data := []byte("hello truncation detection")
	f, err := s.Store("sess1", "f.txt", "text/plain", "agent-a", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sum := sha256.Sum256(data)
	if f.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 mismatch: %s", f.Sha256)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir(), 0)
	data := []byte{0, 1, 2, 250, 251}
	f, err := s.Store("sess1", "f.bin", "application/octet-stream", "agent-a", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if f.Size != 5 {
		t.Fatalf("size mismatch: %d", f.Size)
	}

	fh, got, err := s.Open("sess1", f.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer fh.Close()
	gotBytes, err := io.ReadAll(fh)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(gotBytes, data) {
		t.Fatalf("data corrupted: %v", gotBytes)
	}
	if got.ID != f.ID || got.Name != "f.bin" || got.Sha256 != f.Sha256 {
		t.Fatalf("metadata mismatch: %+v", got)
	}
}

func TestStoreRejectsOversized(t *testing.T) {
	s := NewStore(t.TempDir(), 8)
	_, err := s.Store("sess1", "big.bin", "application/octet-stream", "agent-a", bytes.NewReader(make([]byte, 9)))
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("expected ErrFileTooLarge, got %v", err)
	}
	// The failed write must not leave temp files behind.
	entries, err := os.ReadDir(filepath.Join(s.root, "sess1"))
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover files after rejected store: %v", entries)
	}
}

func TestStoreUnlimitedWhenMaxSizeZero(t *testing.T) {
	s := NewStore(t.TempDir(), 0)
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i % 251)
	}
	f, err := s.Store("sess1", "big.bin", "application/octet-stream", "agent-a", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("store unlimited: %v", err)
	}
	if f.Size != int64(len(big)) {
		t.Fatalf("size mismatch: %d", f.Size)
	}
}

func TestStoreAtExactLimit(t *testing.T) {
	s := NewStore(t.TempDir(), 8)
	if _, err := s.Store("sess1", "ok.bin", "application/octet-stream", "a", bytes.NewReader(make([]byte, 8))); err != nil {
		t.Fatalf("store at exact limit must succeed: %v", err)
	}
}

func TestClearSessionRemovesDir(t *testing.T) {
	s := NewStore(t.TempDir(), 0)
	f, err := s.Store("sess1", "f.bin", "application/octet-stream", "agent-a", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s.ClearSession("sess1")
	if _, ok := s.Get("sess1", f.ID); ok {
		t.Fatal("metadata survived ClearSession")
	}
	if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
		t.Fatal("blob survived ClearSession")
	}
}

func TestDeleteRemovesBlob(t *testing.T) {
	s := NewStore(t.TempDir(), 0)
	f, err := s.Store("sess1", "f.bin", "application/octet-stream", "agent-a", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if !s.Delete("sess1", f.ID) {
		t.Fatal("delete returned false for an existing file")
	}
	if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
		t.Fatal("blob survived Delete")
	}
	if s.Delete("sess1", f.ID) {
		t.Fatal("second delete should be false")
	}
}

func TestRejectsUnsafeIDs(t *testing.T) {
	s := NewStore(t.TempDir(), 0)
	for _, id := range []string{"../evil", "a/b", "", ".", "..", "a.b", "sp ace", strings.Repeat("a", 129)} {
		if _, err := s.Store(id, "x", "application/octet-stream", "a", bytes.NewReader([]byte("x"))); err == nil {
			t.Fatalf("session id %q must be rejected", id)
		}
	}
}

func TestStartupWipesRoot(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "stale-session")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "blob"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	NewStore(root, 0)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("startup wipe left stale blobs behind")
	}
}
