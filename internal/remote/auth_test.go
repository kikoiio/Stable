package remote

import (
	"errors"
	"testing"
	"time"
)

func TestPairingTokenIsSingleUseAndSessionCanBeRevoked(t *testing.T) {
	store := newPairingStore(time.Minute)
	token, expires, err := store.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || !expires.After(time.Now()) {
		t.Fatalf("Issue() = token %q expires %v", token, expires)
	}
	session, err := store.Consume(token)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || session.Credential == "" {
		t.Fatalf("Consume() returned incomplete session: %+v", session)
	}
	if got, ok := store.Authenticate(session.Credential); !ok || got.ID != session.ID {
		t.Fatalf("Authenticate() = %+v, %v", got, ok)
	}
	if _, err := store.Consume(token); !errors.Is(err, errInvalidPairingToken) {
		t.Fatalf("second Consume() error = %v", err)
	}
	store.Revoke(session.ID)
	if _, ok := store.Authenticate(session.Credential); ok {
		t.Fatal("revoked session still authenticated")
	}
}

func TestPairingTokenExpiryAndClear(t *testing.T) {
	store := newPairingStore(time.Minute)
	now := time.Now()
	store.now = func() time.Time { return now }
	token, _, err := store.Issue()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := store.Consume(token); !errors.Is(err, errInvalidPairingToken) {
		t.Fatalf("expired Consume() error = %v", err)
	}
	now = time.Now()
	token, _, err = store.Issue()
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Consume(token)
	if err != nil {
		t.Fatal(err)
	}
	store.Clear()
	if _, ok := store.Authenticate(session.Credential); ok {
		t.Fatal("session remained valid after Clear")
	}
	if _, err := store.Consume(token); !errors.Is(err, errInvalidPairingToken) {
		t.Fatalf("token remained valid after Clear: %v", err)
	}
}

func TestPairRateLimiterCooldown(t *testing.T) {
	limiter := NewPairRateLimiter()
	now := time.Now()
	limiter.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		if !limiter.Allow("127.0.0.1") {
			t.Fatalf("source blocked before attempt %d", i+1)
		}
		limiter.Failure("127.0.0.1")
	}
	if !limiter.Allow("127.0.0.1") {
		t.Fatal("source blocked before fifth failure")
	}
	limiter.Failure("127.0.0.1")
	if limiter.Allow("127.0.0.1") {
		t.Fatal("source was not blocked after fifth failure")
	}
	if !limiter.Allow("127.0.0.2") {
		t.Fatal("one source's failures blocked another source")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("127.0.0.1") {
		t.Fatal("source remained blocked after cooldown")
	}
}
