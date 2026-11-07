package filestore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var ErrFileTooLarge = errors.New("file exceeds maximum allowed size")

// safeID guards the on-disk layout. Session IDs are client-choosable (via
// register's GetOrCreate), and both session IDs and file IDs become path
// components under the store root, so anything outside [A-Za-z0-9_-] is
// rejected before it can reach the filesystem.
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type File struct {
	ID          string `json:"id"`
	Name        string `json:"file_name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// Sha256 is the hex digest of the stored bytes, computed once at upload,
	// so receivers can verify a transfer end-to-end.
	Sha256 string `json:"sha256"`
	// Path is the blob's location on disk — internal, never serialized to
	// API clients.
	Path       string    `json:"-"`
	UploadedBy string    `json:"uploaded_by"`
	UploadedAt time.Time `json:"uploaded_at"`
}

type Store struct {
	mu      sync.RWMutex
	root    string
	maxSize int64 // <= 0: unlimited
	files   map[string]map[string]*File
}

// NewStore creates a disk-backed file store under root. Files are
// session-scoped and not persisted: anything left in root from a previous
// run points at state that no longer exists, so it is wiped at startup —
// the same lost-on-restart contract as when blobs lived in memory.
// maxFileSize <= 0 means unlimited.
func NewStore(root string, maxFileSize int64) *Store {
	_ = os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		// Every Store() call will surface this with its own error; keep
		// going so the failure names the operation that needed the dir.
		fmt.Fprintf(os.Stderr, "filestore: cannot create root %s: %v\n", root, err)
	}
	return &Store{
		root:    root,
		maxSize: maxFileSize,
		files:   make(map[string]map[string]*File),
	}
}

func (s *Store) MaxSize() int64 {
	return s.maxSize
}

// Store streams r to disk under the session's directory, computing the
// SHA-256 while copying — the body is never held in memory. The blob is
// written to a temp file and renamed into place, so a partial write never
// becomes a stored file.
func (s *Store) Store(sessionID, filename, contentType, uploadedBy string, r io.Reader) (*File, error) {
	if !safeID.MatchString(sessionID) {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}

	dir := filepath.Join(s.root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session file dir: %w", err)
	}

	limited := r
	if s.maxSize > 0 {
		// Read one byte past the cap so an over-sized body is detected as
		// such (n > maxSize) rather than silently truncated.
		limited = io.LimitReader(r, s.maxSize+1)
	}

	id := generateID()
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed

	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), limited)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}
	if s.maxSize > 0 && n > s.maxSize {
		return nil, ErrFileTooLarge
	}

	f := &File{
		ID:          id,
		Name:        filename,
		ContentType: contentType,
		Size:        n,
		Sha256:      hex.EncodeToString(hash.Sum(nil)),
		Path:        filepath.Join(dir, id),
		UploadedBy:  uploadedBy,
		UploadedAt:  time.Now().UTC(),
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return nil, fmt.Errorf("finalize file: %w", err)
	}

	s.mu.Lock()
	if s.files[sessionID] == nil {
		s.files[sessionID] = make(map[string]*File)
	}
	s.files[sessionID][id] = f
	s.mu.Unlock()
	return f, nil
}

func (s *Store) Get(sessionID, fileID string) (*File, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.files[sessionID] == nil {
		return nil, false
	}
	f, ok := s.files[sessionID][fileID]
	if !ok {
		return nil, false
	}
	return f, true
}

// Open returns the metadata plus an open handle on the blob for streaming
// downloads. The caller owns the handle and must close it.
func (s *Store) Open(sessionID, fileID string) (*os.File, *File, error) {
	if !safeID.MatchString(sessionID) || !safeID.MatchString(fileID) {
		return nil, nil, fmt.Errorf("invalid id")
	}
	s.mu.RLock()
	f, ok := s.files[sessionID][fileID]
	s.mu.RUnlock()
	if !ok {
		return nil, nil, os.ErrNotExist
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, nil, err
	}
	return file, f, nil
}

func (s *Store) Delete(sessionID, fileID string) bool {
	s.mu.Lock()
	var path string
	if s.files[sessionID] != nil {
		if f, ok := s.files[sessionID][fileID]; ok {
			path = f.Path
			delete(s.files[sessionID], fileID)
		}
	}
	s.mu.Unlock()

	if path == "" {
		return false
	}
	// Metadata is already gone; a failed unlink would only leave an
	// unreferenced blob, which janitor cleanup will collect.
	_ = os.Remove(path)
	return true
}

func (s *Store) List(sessionID string) []*File {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.files[sessionID] == nil {
		return nil
	}
	out := make([]*File, 0, len(s.files[sessionID]))
	for _, f := range s.files[sessionID] {
		out = append(out, f)
	}
	return out
}

// ClearSession drops the session's metadata and removes its blob directory.
func (s *Store) ClearSession(sessionID string) {
	s.mu.Lock()
	delete(s.files, sessionID)
	s.mu.Unlock()
	_ = os.RemoveAll(filepath.Join(s.root, sessionID))
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
