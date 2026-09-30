package filestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestStoreComputesSha256(t *testing.T) {
	s := NewStore(1 << 20)
	data := []byte("hello truncation detection")
	f, err := s.Store("s", "f.txt", "text/plain", "agent-a", data)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sum := sha256.Sum256(data)
	if f.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 mismatch: %s", f.Sha256)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(1 << 20)
	f, err := s.Store("s", "f.bin", "application/octet-stream", "agent-a", []byte{0, 1, 2, 250, 251})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	got, ok := s.Get("s", f.ID)
	if !ok {
		t.Fatal("file not found after store")
	}
	if !bytes.Equal(got.Data, []byte{0, 1, 2, 250, 251}) {
		t.Fatalf("data corrupted: %v", got.Data)
	}
	if got.Size != 5 {
		t.Fatalf("size mismatch: %d", got.Size)
	}
}
