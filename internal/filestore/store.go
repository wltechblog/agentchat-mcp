package filestore

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type File struct {
	ID          string
	Name        string
	ContentType string
	Size        int64
	Data        []byte
	UploadedBy  string
	UploadedAt  time.Time
}

type Store struct {
	mu      sync.RWMutex
	files   map[string]map[string]*File
	maxSize int64
}

func NewStore(maxFileSize int64) *Store {
	return &Store{
		files:   make(map[string]map[string]*File),
		maxSize: maxFileSize,
	}
}

func (s *Store) MaxSize() int64 {
	return s.maxSize
}

func (s *Store) Store(sessionID, filename, contentType, uploadedBy string, data []byte) (*File, error) {
	if int64(len(data)) > s.maxSize {
		return nil, ErrFileTooLarge
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.files[sessionID] == nil {
		s.files[sessionID] = make(map[string]*File)
	}

	id := generateID()
	f := &File{
		ID:          id,
		Name:        filename,
		ContentType: contentType,
		Size:        int64(len(data)),
		Data:        data,
		UploadedBy:  uploadedBy,
		UploadedAt:  time.Now().UTC(),
	}
	s.files[sessionID][id] = f
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

func (s *Store) Delete(sessionID, fileID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.files[sessionID] == nil {
		return false
	}
	if _, ok := s.files[sessionID][fileID]; !ok {
		return false
	}
	delete(s.files[sessionID], fileID)
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
		out = append(out, &File{
			ID:          f.ID,
			Name:        f.Name,
			ContentType: f.ContentType,
			Size:        f.Size,
			UploadedBy:  f.UploadedBy,
			UploadedAt:  f.UploadedAt,
		})
	}
	return out
}

func (s *Store) ClearSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, sessionID)
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
