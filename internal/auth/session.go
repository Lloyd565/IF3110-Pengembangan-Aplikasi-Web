package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

var ErrInvalidSession = errors.New("auth: session tidak valid atau kedaluwarsa")

type Policy struct {
	IdleTimeout      time.Duration
	AbsoluteTTL      time.Duration
	RotationInterval time.Duration
	GracePeriod      time.Duration
}

func DefaultPolicy() Policy {
	return Policy{30 * time.Minute, 8 * time.Hour, 15 * time.Minute, 30 * time.Second}
}

func (p Policy) Validate() error {
	if p.IdleTimeout <= 0 || p.AbsoluteTTL <= 0 || p.RotationInterval <= 0 || p.GracePeriod <= 0 ||
		p.GracePeriod >= p.RotationInterval || p.RotationInterval >= p.AbsoluteTTL || p.IdleTimeout > p.AbsoluteTTL {
		return errors.New("session: wajib 0 < grace < rotation < absolute dan 0 < idle <= absolute")
	}
	return nil
}

type Session struct {
	ID                int64
	Principal         reqctx.Principal
	Active            bool
	TokenHash         [32]byte
	PrevTokenHash     []byte
	PrevValidUntil    *time.Time
	CSRFToken         string
	ForceRotate       bool
	CreatedAt         time.Time
	LastSeenAt        time.Time
	RotatedAt         time.Time
	AbsoluteExpiresAt time.Time
	RevokedAt         *time.Time
}

type SessionStore interface {
	Create(context.Context, *Session) error
	Find(context.Context, [32]byte) (Session, error)
	CompareAndSwap(context.Context, Session, Session) (bool, error)
	Expire(context.Context, int64, time.Time, time.Time) (bool, error)
	Revoke(context.Context, int64, time.Time) error
}

type Resolution struct {
	Session Session
	// Token is set only when a new cookie must be sent.
	Token string
}

type Manager struct {
	store  SessionStore
	policy Policy
	key    []byte
	now    func() time.Time
}

func NewManager(store SessionStore, policy Policy, key []byte) (*Manager, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if store == nil || len(key) != 32 {
		return nil, errors.New("session: store dan key 32 byte wajib diisi")
	}
	return &Manager{store: store, policy: policy, key: append([]byte(nil), key...), now: time.Now}, nil
}

func (m *Manager) Issue(ctx context.Context, p reqctx.Principal) (Resolution, error) {
	token, err := NewToken()
	if err != nil {
		return Resolution{}, err
	}
	csrf, err := NewToken()
	if err != nil {
		return Resolution{}, err
	}
	now := m.now().UTC().Truncate(time.Microsecond)
	s := Session{Principal: p, Active: true, TokenHash: TokenHash(token), CSRFToken: csrf,
		CreatedAt: now, LastSeenAt: now, RotatedAt: now, AbsoluteExpiresAt: now.Add(m.policy.AbsoluteTTL)}
	if err := m.store.Create(ctx, &s); err != nil {
		return Resolution{}, err
	}
	return Resolution{Session: s, Token: token}, nil
}

func (m *Manager) Resolve(ctx context.Context, token string) (Resolution, error) {
	if !ValidToken(token) {
		return Resolution{}, ErrInvalidSession
	}
	hash := TokenHash(token)
	for range 16 {
		s, err := m.store.Find(ctx, hash)
		if err != nil {
			return Resolution{}, err
		}
		now := m.now().UTC().Truncate(time.Microsecond)
		if s.RevokedAt != nil {
			return Resolution{}, ErrInvalidSession
		}
		if !s.Active || !now.Before(s.AbsoluteExpiresAt) || !now.Before(s.LastSeenAt.Add(m.policy.IdleTimeout)) {
			expired, err := m.store.Expire(ctx, s.ID, now, now.Add(-m.policy.IdleTimeout))
			if err != nil {
				return Resolution{}, err
			}
			if expired {
				return Resolution{}, ErrInvalidSession
			}
			// A concurrent request may have refreshed last_seen after Find.
			continue
		}
		current := subtle.ConstantTimeCompare(hash[:], s.TokenHash[:]) == 1
		if !current && (s.PrevValidUntil == nil || !now.Before(*s.PrevValidUntil) || subtle.ConstantTimeCompare(hash[:], s.PrevTokenHash) != 1) {
			return Resolution{}, ErrInvalidSession
		}
		next := s
		if now.After(next.LastSeenAt) {
			next.LastSeenAt = now
		}
		var replacement string
		if current && (s.ForceRotate || !now.Before(s.RotatedAt.Add(m.policy.RotationInterval))) {
			replacement = rotatedToken(m.key, token, s.ID, now)
			next.PrevTokenHash = append([]byte(nil), s.TokenHash[:]...)
			until := now.Add(m.policy.GracePeriod)
			next.PrevValidUntil = &until
			next.TokenHash = TokenHash(replacement)
			next.RotatedAt, next.ForceRotate = now, false
		} else if !current {
			// CAS losers and clients that missed Set-Cookie recover the winning value.
			candidate := rotatedToken(m.key, token, s.ID, s.RotatedAt)
			candidateHash := TokenHash(candidate)
			if subtle.ConstantTimeCompare(candidateHash[:], s.TokenHash[:]) == 1 {
				replacement = candidate
			}
		}
		ok, err := m.store.CompareAndSwap(ctx, s, next)
		if err != nil {
			return Resolution{}, err
		}
		if ok {
			return Resolution{Session: next, Token: replacement}, nil
		}
	}
	return Resolution{}, errors.New("session: concurrent update limit reached")
}

func (m *Manager) Revoke(ctx context.Context, id int64) error {
	return m.store.Revoke(ctx, id, m.now().UTC())
}
