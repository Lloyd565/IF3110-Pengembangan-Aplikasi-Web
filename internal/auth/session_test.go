package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

type fakeStore struct {
	mu            sync.Mutex
	session       Session
	fail          bool
	loseOnce      bool
	storeError    error
	failOperation string
	alwaysLose    bool
}

func (s *fakeStore) Create(_ context.Context, session *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOperation == "create" {
		return s.storeError
	}
	session.ID = 1
	s.session = *session
	return nil
}
func (s *fakeStore) Find(_ context.Context, hash [32]byte) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return Session{}, errors.New("database offline")
	}
	if s.failOperation == "find" {
		return Session{}, s.storeError
	}
	if s.session.TokenHash != hash && !EqualCSRF(string(s.session.PrevTokenHash), string(hash[:])) {
		return Session{}, ErrInvalidSession
	}
	return s.session, nil
}
func (s *fakeStore) CompareAndSwap(_ context.Context, old, next Session) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOperation == "cas" {
		return false, s.storeError
	}
	if s.alwaysLose {
		return false, nil
	}
	if s.loseOnce {
		s.loseOnce = false
		return false, nil
	}
	if s.session.TokenHash != old.TokenHash || s.session.ForceRotate != old.ForceRotate || s.session.RevokedAt != nil || !s.session.Active {
		return false, nil
	}
	if s.session.LastSeenAt.After(next.LastSeenAt) {
		next.LastSeenAt = s.session.LastSeenAt
	}
	s.session = next
	return true, nil
}
func (s *fakeStore) Expire(_ context.Context, _ int64, now, idleCutoff time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOperation == "expire" {
		return false, s.storeError
	}
	if s.session.RevokedAt == nil && (!s.session.Active || !s.session.AbsoluteExpiresAt.After(now) || !s.session.LastSeenAt.After(idleCutoff)) {
		s.session.RevokedAt = &now
		return true, nil
	}
	return false, nil
}
func (s *fakeStore) Revoke(_ context.Context, _ int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOperation == "revoke" {
		return s.storeError
	}
	s.session.RevokedAt = &now
	return nil
}

func sessionFixture(t *testing.T) (*Manager, *fakeStore, *time.Time, Resolution) {
	t.Helper()
	store := &fakeStore{}
	m, err := NewManager(store, DefaultPolicy(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	r, err := m.Issue(context.Background(), reqctx.Principal{UserID: 1, Username: "budi"})
	if err != nil {
		t.Fatal(err)
	}
	return m, store, &now, r
}

func TestSessionIssueAndTimeouts(t *testing.T) {
	for _, name := range []string{"valid", "idle boundary", "absolute boundary", "revoked", "inactive", "unknown", "malformed", "backend failure"} {
		t.Run(name, func(t *testing.T) {
			m, store, now, issued := sessionFixture(t)
			token := issued.Token
			if !ValidToken(token) || !ValidToken(issued.Session.CSRFToken) || TokenHash(token) != issued.Session.TokenHash {
				t.Fatal("token contract")
			}
			switch name {
			case "idle boundary":
				*now = now.Add(30 * time.Minute)
			case "absolute boundary":
				*now = now.Add(8 * time.Hour)
				store.session.LastSeenAt = *now
			case "revoked":
				store.session.RevokedAt = now
			case "inactive":
				store.session.Active = false
			case "unknown":
				token, _ = NewToken()
			case "malformed":
				token = "invalid"
			case "backend failure":
				store.fail = true
			}
			_, err := m.Resolve(context.Background(), token)
			if name == "valid" && err != nil {
				t.Fatal(err)
			}
			if name != "valid" && err == nil {
				t.Fatal("invalid session accepted")
			}
			if name == "backend failure" && errors.Is(err, ErrInvalidSession) {
				t.Fatal("backend errors must remain distinguishable")
			}
		})
	}
}

func TestSessionRotationGraceAndAbsolute(t *testing.T) {
	m, store, now, issued := sessionFixture(t)
	*now = now.Add(14 * time.Minute)
	r, err := m.Resolve(context.Background(), issued.Token)
	if err != nil || r.Token != "" {
		t.Fatalf("early rotation: %v", err)
	}
	*now = now.Add(time.Minute)
	r, err = m.Resolve(context.Background(), issued.Token)
	if err != nil || r.Token == "" || r.Token == issued.Token {
		t.Fatalf("rotation: %v", err)
	}
	if !r.Session.AbsoluteExpiresAt.Equal(issued.Session.AbsoluteExpiresAt) {
		t.Fatal("rotation extended absolute expiry")
	}
	if r.Session.CSRFToken != issued.Session.CSRFToken {
		t.Fatal("rotation broke existing forms")
	}
	winner := r.Token
	*now = now.Add(29 * time.Second)
	r, err = m.Resolve(context.Background(), issued.Token)
	if err != nil || r.Token != winner {
		t.Fatalf("grace must recover winner: %v", err)
	}
	*now = now.Add(time.Second)
	if _, err := m.Resolve(context.Background(), issued.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("grace boundary: %v", err)
	}
	if _, err := m.Resolve(context.Background(), winner); err != nil {
		t.Fatal(err)
	}
	store.session.ForceRotate = true
	r, err = m.Resolve(context.Background(), winner)
	if err != nil || r.Token == "" || r.Token == winner || r.Session.ForceRotate {
		t.Fatalf("forced rotation: %v", err)
	}
	if err := m.Revoke(context.Background(), r.Session.ID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{winner, r.Token} {
		if _, err := m.Resolve(context.Background(), token); !errors.Is(err, ErrInvalidSession) {
			t.Fatalf("logout replay: %v", err)
		}
	}
}

func TestSessionConcurrentRotationCAS(t *testing.T) {
	m, store, now, issued := sessionFixture(t)
	*now = now.Add(15 * time.Minute)
	store.loseOnce = true
	var wg sync.WaitGroup
	results := make(chan Resolution, 16)
	errorsCh := make(chan error, 16)
	for range 16 {
		wg.Go(func() { r, err := m.Resolve(context.Background(), issued.Token); results <- r; errorsCh <- err })
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	winner := ""
	for r := range results {
		if winner == "" {
			winner = r.Token
		}
		if winner == "" || r.Token != winner {
			t.Fatal("parallel responses disagree on winner")
		}
	}
	if store.session.TokenHash != TokenHash(winner) {
		t.Fatal("winner is not persisted")
	}
}

type staleSnapshotStore struct {
	*fakeStore
	snapshot *Session
}

func (s *staleSnapshotStore) Find(ctx context.Context, hash [32]byte) (Session, error) {
	if s.snapshot != nil {
		old := *s.snapshot
		s.snapshot = nil
		return old, nil
	}
	return s.fakeStore.Find(ctx, hash)
}

func TestSessionStaleExpiryDoesNotRevokeLiveSession(t *testing.T) {
	m, store, now, issued := sessionFixture(t)
	*now = now.Add(30 * time.Minute)
	// A concurrent request refreshed last_seen after this request read its snapshot.
	store.session.LastSeenAt = *now
	m.store = &staleSnapshotStore{fakeStore: store, snapshot: &issued.Session}
	if _, err := m.Resolve(context.Background(), issued.Token); err != nil {
		t.Fatalf("a stale idle snapshot revoked a live session: %v", err)
	}
	if store.session.RevokedAt != nil {
		t.Fatal("live session was revoked")
	}
}

func TestSessionStoreFailures(t *testing.T) {
	backendError := errors.New("simulated backend failure")
	for _, operation := range []string{"create", "find", "cas", "expire", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			m, store, now, issued := sessionFixture(t)
			store.failOperation, store.storeError = operation, backendError
			var err error
			switch operation {
			case "create":
				_, err = m.Issue(context.Background(), issued.Session.Principal)
			case "revoke":
				err = m.Revoke(context.Background(), issued.Session.ID)
			case "expire":
				*now = now.Add(30 * time.Minute)
				_, err = m.Resolve(context.Background(), issued.Token)
			default:
				_, err = m.Resolve(context.Background(), issued.Token)
			}
			if !errors.Is(err, backendError) {
				t.Fatalf("store failure must be propagated: %v", err)
			}
		})
	}
	t.Run("bounded CAS retries", func(t *testing.T) {
		m, store, _, issued := sessionFixture(t)
		store.alwaysLose = true
		if _, err := m.Resolve(context.Background(), issued.Token); err == nil {
			t.Fatal("unbounded/failed CAS was accepted")
		}
	})
}

func TestSessionPolicyValidation(t *testing.T) {
	for _, p := range []Policy{
		{}, {0, 8 * time.Hour, 15 * time.Minute, 30 * time.Second},
		{30 * time.Minute, 8 * time.Hour, 15 * time.Minute, 15 * time.Minute},
		{9 * time.Hour, 8 * time.Hour, 15 * time.Minute, 30 * time.Second},
		{30 * time.Minute, 8 * time.Hour, 8 * time.Hour, 30 * time.Second},
	} {
		if p.Validate() == nil {
			t.Fatal("invalid policy was accepted")
		}
	}
	if _, err := NewManager(nil, DefaultPolicy(), make([]byte, 32)); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewManager(&fakeStore{}, DefaultPolicy(), make([]byte, 31)); err == nil {
		t.Fatal("short signing key accepted")
	}
	if _, err := NewManager(&fakeStore{}, Policy{}, make([]byte, 32)); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
