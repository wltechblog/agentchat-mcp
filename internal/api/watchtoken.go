package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// watchTokenStore issues short-lived tokens so /watch URLs don't carry the
// session PSK — query strings end up in proxy and access logs. Tokens are
// issued by POST /register and renewed on every heartbeat.
type watchTokenStore struct {
	mu     sync.Mutex
	tokens map[string]watchToken
	ttl    time.Duration
}

type watchToken struct {
	sessionID string
	expires   time.Time
}

func newWatchTokenStore(ttl time.Duration) *watchTokenStore {
	return &watchTokenStore{
		tokens: make(map[string]watchToken),
		ttl:    ttl,
	}
}

// Issue creates a new token for a session, pruning expired ones. Any active
// token keeps working until it expires; tokens are bearer credentials for
// watching one session and nothing more.
func (s *watchTokenStore) Issue(sessionID string) string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failure is unrecoverable; panic rather than issue a
		// predictable token.
		panic("watch token: crypto/rand failed: " + err.Error())
	}
	tok := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, t := range s.tokens {
		if now.After(t.expires) {
			delete(s.tokens, k)
		}
	}
	s.tokens[tok] = watchToken{sessionID: sessionID, expires: now.Add(s.ttl)}
	return tok
}

// Validate reports whether the token is a live token for the session.
func (s *watchTokenStore) Validate(token, sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[token]
	return ok && t.sessionID == sessionID && time.Now().Before(t.expires)
}
