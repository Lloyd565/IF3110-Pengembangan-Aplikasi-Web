package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func TestIntegrationCredentialsAndSessions(t *testing.T) {
	db := testdb.Fresh(t, "auth_sessions")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword("rahasia-untuk-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (id, username, password_hash) VALUES (1, 'Budi', $1)`, hash); err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(db)
	if err != nil {
		t.Fatal(err)
	}
	p, err := credentials.Authenticate(ctx, "bUdI", "rahasia-untuk-test")
	if err != nil || p.UserID != 1 {
		t.Fatalf("login case insensitive: %v", err)
	}
	for _, username := range []string{"Budi", "unknown"} {
		if _, err := credentials.Authenticate(ctx, username, "wrong"); !errors.Is(err, ErrCredentials) {
			t.Fatalf("invalid credentials: %v", err)
		}
	}
	store := &Sessions{DB: db}
	manager, err := NewManager(store, DefaultPolicy(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 0, 0, 0, 123456000, time.UTC)
	manager.now = func() time.Time { return now }
	issued, err := manager.Issue(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(ctx, issued.Token); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	var wg sync.WaitGroup
	results := make(chan Resolution, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() { r, err := manager.Resolve(ctx, issued.Token); results <- r; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var winner string
	for r := range results {
		if winner == "" {
			winner = r.Token
		}
		if winner == "" || r.Token != winner || !r.Session.AbsoluteExpiresAt.Equal(issued.Session.AbsoluteExpiresAt) {
			t.Fatal("CAS/absolute contract")
		}
	}
	now = now.Add(30 * time.Second)
	if _, err := manager.Resolve(ctx, issued.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("old token after grace: %v", err)
	}
	if err := store.MarkForceRotate(ctx, p.UserID); err != nil {
		t.Fatal(err)
	}
	r, err := manager.Resolve(ctx, winner)
	if err != nil || r.Token == "" || r.Token == winner {
		t.Fatalf("privilege rotation: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE users SET is_platform_admin = true WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	r2, err := manager.Resolve(ctx, r.Token)
	if err != nil || !r2.Session.Principal.IsPlatformAdmin {
		t.Fatal("principal must be reloaded on each request", err)
	}
	if err := store.RevokeAllForUser(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(ctx, r.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoke all: %v", err)
	}
	issued, err = manager.Issue(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE users SET is_active = false WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(ctx, issued.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("inactive user session accepted", err)
	}
	if _, err := credentials.Authenticate(ctx, "Budi", "rahasia-untuk-test"); !errors.Is(err, ErrCredentials) {
		t.Fatal("inactive user login accepted", err)
	}
}

func TestIntegrationSessionAtomicRefreshAndExpiry(t *testing.T) {
	db := testdb.Fresh(t, "auth_cas")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (id, username, password_hash) VALUES (1, 'Budi', 'unused')`); err != nil {
		t.Fatal(err)
	}
	store := &Sessions{DB: db}
	manager, err := NewManager(store, DefaultPolicy(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := manager.Issue(ctx, reqctx.Principal{UserID: 1, Username: "Budi"})
	if err != nil {
		t.Fatal(err)
	}
	old := issued.Session
	fresh := old
	fresh.LastSeenAt = old.LastSeenAt.Add(10 * time.Minute)
	if ok, err := store.CompareAndSwap(ctx, old, fresh); err != nil || !ok {
		t.Fatal("initial refresh failed", err)
	}
	staleUpdate := old
	staleUpdate.LastSeenAt = old.LastSeenAt.Add(time.Minute)
	if ok, err := store.CompareAndSwap(ctx, old, staleUpdate); err != nil || !ok {
		t.Fatal("parallel touch unnecessarily conflicted", err)
	}
	live, err := store.Find(ctx, old.TokenHash)
	if err != nil || !live.LastSeenAt.Equal(fresh.LastSeenAt) {
		t.Fatal("last_seen moved backwards", err)
	}
	at := old.LastSeenAt.Add(35 * time.Minute)
	if expired, err := store.Expire(ctx, old.ID, at, at.Add(-30*time.Minute)); err != nil || expired {
		t.Fatal("live session incorrectly expired", err)
	}
	if err := store.MarkForceRotate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.CompareAndSwap(ctx, old, staleUpdate); err != nil || ok {
		t.Fatal("stale request consumed force_rotate", err)
	}
	at = old.LastSeenAt.Add(40 * time.Minute)
	if expired, err := store.Expire(ctx, old.ID, at, at.Add(-30*time.Minute)); err != nil || !expired {
		t.Fatal("idle boundary not expired", err)
	}
	if ok, err := store.CompareAndSwap(ctx, old, fresh); err != nil || ok {
		t.Fatal("revoked session was resurrected", err)
	}
}
