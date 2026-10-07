package remote

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

var (
	errInvalidPairingToken = errors.New("invalid or expired pairing token")
	errInvalidSession      = errors.New("invalid browser session")
)

const defaultPairTTL = 5 * time.Minute

// BrowserSession contains an opaque cookie credential. Credential is only
// returned at pairing time and must never be logged or serialized.
type BrowserSession struct {
	ID         string
	Credential string
}

// PairingStore issues one-use pairing tokens and keeps only hashes of pairing
// tokens and browser credentials in memory.
type PairingStore interface {
	Issue() (token string, expiresAt time.Time, err error)
	Consume(token string) (BrowserSession, error)
	Revoke(sessionID string)
}

type pairingStore struct {
	mu       sync.Mutex
	pairings map[[32]byte]time.Time
	sessions map[[32]byte]string // credential digest -> session ID
	pairTTL  time.Duration
	now      func() time.Time
}

func newPairingStore(ttl time.Duration) *pairingStore {
	if ttl <= 0 {
		ttl = defaultPairTTL
	}
	return &pairingStore{
		pairings: make(map[[32]byte]time.Time),
		sessions: make(map[[32]byte]string),
		pairTTL:  ttl,
		now:      time.Now,
	}
}

func (s *pairingStore) Issue() (string, time.Time, error) {
	token, err := randomCredential()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	expires := now.Add(s.pairTTL)
	s.mu.Lock()
	s.prunePairingsLocked(now)
	s.pairings[digest("stable/pairing/v1", token)] = expires
	s.mu.Unlock()
	return token, expires, nil
}

func (s *pairingStore) Consume(token string) (BrowserSession, error) {
	if token == "" {
		return BrowserSession{}, errInvalidPairingToken
	}
	now := s.now()
	pairingDigest := digest("stable/pairing/v1", token)
	s.mu.Lock()
	expires, ok := s.pairings[pairingDigest]
	if ok {
		delete(s.pairings, pairingDigest)
	}
	if !ok || !now.Before(expires) {
		s.mu.Unlock()
		return BrowserSession{}, errInvalidPairingToken
	}
	credential, err := randomCredential()
	if err != nil {
		s.mu.Unlock()
		return BrowserSession{}, err
	}
	// The session ID is independent of the cookie secret. Both values are
	// random, and only the digest of the cookie secret is retained.
	id, err := randomCredential()
	if err != nil {
		s.mu.Unlock()
		return BrowserSession{}, err
	}
	s.sessions[digest("stable/browser-session/v1", credential)] = id
	s.mu.Unlock()
	return BrowserSession{ID: id, Credential: credential}, nil
}

func (s *pairingStore) Authenticate(credential string) (BrowserSession, bool) {
	if credential == "" {
		return BrowserSession{}, false
	}
	d := digest("stable/browser-session/v1", credential)
	s.mu.Lock()
	id, ok := s.sessions[d]
	s.mu.Unlock()
	if !ok {
		return BrowserSession{}, false
	}
	return BrowserSession{ID: id}, true
}

func (s *pairingStore) Revoke(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	for digest, id := range s.sessions {
		if id == sessionID {
			delete(s.sessions, digest)
		}
	}
	s.mu.Unlock()
}

// Clear invalidates every pairing token and browser session. It is called
// whenever the RemoteManager stops, including runtime shutdown.
func (s *pairingStore) Clear() {
	s.mu.Lock()
	s.pairings = make(map[[32]byte]time.Time)
	s.sessions = make(map[[32]byte]string)
	s.mu.Unlock()
}

func (s *pairingStore) prunePairingsLocked(now time.Time) {
	for tokenDigest, expires := range s.pairings {
		if !now.Before(expires) {
			delete(s.pairings, tokenDigest)
		}
	}
}

func randomCredential() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func digest(purpose, value string) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(purpose))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(value))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

type rateWindow struct {
	started time.Time
	count   int
	blocked time.Time
}

// PairRateLimiter allows five failed pairing attempts per source in each
// minute and blocks that source for one minute after the fifth failure.
type PairRateLimiter struct {
	mu      sync.Mutex
	entries map[string]rateWindow
	window  time.Duration
	limit   int
	coolOff time.Duration
	now     func() time.Time
}

func NewPairRateLimiter() *PairRateLimiter {
	return &PairRateLimiter{
		entries: make(map[string]rateWindow),
		window:  time.Minute,
		limit:   5,
		coolOff: time.Minute,
		now:     time.Now,
	}
}

func (l *PairRateLimiter) Allow(source string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[source]
	if !ok {
		return true
	}
	if now.Before(entry.blocked) {
		return false
	}
	if !entry.blocked.IsZero() || !now.Before(entry.started.Add(l.window)) {
		delete(l.entries, source)
	}
	return true
}

func (l *PairRateLimiter) Failure(source string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[source]
	if !ok || !now.Before(entry.started.Add(l.window)) || !entry.blocked.IsZero() && !now.Before(entry.blocked) {
		entry = rateWindow{started: now}
	}
	entry.count++
	if entry.count >= l.limit {
		entry.blocked = now.Add(l.coolOff)
	}
	l.entries[source] = entry
}
