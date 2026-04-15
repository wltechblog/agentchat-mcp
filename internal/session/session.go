package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/auth"
)

type Session struct {
	ID        string
	Name      string
	PSK       string
	CreatedAt time.Time
}

type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewStore() *Store {
	return &Store{
		sessions: make(map[string]*Session),
	}
}

func (s *Store) Create(name string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := generateID()
	sess := &Session{
		ID:        id,
		Name:      name,
		PSK:       auth.GeneratePSK(),
		CreatedAt: time.Now().UTC(),
	}
	s.sessions[id] = sess
	return sess
}

func (s *Store) Get(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false
	}
	delete(s.sessions, id)
	return true
}

func (s *Store) ValidatePSK(sessionID, psk string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if sess.PSK != psk {
		return nil, false
	}
	return sess, true
}

func (s *Store) GetOrCreate(sessionID, psk, name string) (*Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess, ok := s.sessions[sessionID]; ok {
		if sess.PSK != psk {
			return nil, false, nil
		}
		return sess, false, nil
	}

	if name == "" {
		name = sessionID
	}
	sess := &Session{
		ID:        sessionID,
		Name:      name,
		PSK:       psk,
		CreatedAt: time.Now().UTC(),
	}
	s.sessions[sessionID] = sess
	return sess, true, nil
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
