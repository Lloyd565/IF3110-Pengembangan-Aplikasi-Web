package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Sessions struct {
	DB *pgxpool.Pool
}

func (s *Sessions) Create(ctx context.Context, session *Session) error {
	err := s.DB.QueryRow(ctx, `INSERT INTO sessions
		(user_id, token_hash, csrf_token, created_at, last_seen_at, rotated_at, absolute_expires_at)
		SELECT id, $2, $3, $4, $4, $4, $5 FROM users WHERE id = $1 AND is_active
		RETURNING id`, session.Principal.UserID, session.TokenHash[:], session.CSRFToken,
		session.CreatedAt, session.AbsoluteExpiresAt).Scan(&session.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidSession
	}
	return err
}

func (s *Sessions) Find(ctx context.Context, hash [32]byte) (Session, error) {
	var session Session
	var current []byte
	err := s.DB.QueryRow(ctx, `SELECT s.id, u.id, u.username, u.is_platform_admin, u.is_active,
		s.token_hash, s.prev_token_hash, s.prev_valid_until, s.csrf_token, s.force_rotate,
		s.created_at, s.last_seen_at, s.rotated_at, s.absolute_expires_at, s.revoked_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 OR s.prev_token_hash = $1`, hash[:]).Scan(
		&session.ID, &session.Principal.UserID, &session.Principal.Username, &session.Principal.IsPlatformAdmin, &session.Active,
		&current, &session.PrevTokenHash, &session.PrevValidUntil, &session.CSRFToken, &session.ForceRotate,
		&session.CreatedAt, &session.LastSeenAt, &session.RotatedAt, &session.AbsoluteExpiresAt, &session.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrInvalidSession
	}
	if err != nil {
		return Session{}, err
	}
	if len(current) != 32 {
		return Session{}, ErrInvalidSession
	}
	copy(session.TokenHash[:], current)
	return session, nil
}

func (s *Sessions) CompareAndSwap(ctx context.Context, old, next Session) (bool, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE sessions s SET token_hash = $2, prev_token_hash = $3,
		prev_valid_until = $4, last_seen_at = GREATEST(s.last_seen_at, $5), rotated_at = $6, force_rotate = $7
		WHERE s.id = $1 AND s.token_hash = $8 AND s.force_rotate = $9
		AND s.revoked_at IS NULL AND EXISTS (SELECT 1 FROM users u WHERE u.id = s.user_id AND u.is_active)`,
		next.ID, next.TokenHash[:], next.PrevTokenHash, next.PrevValidUntil, next.LastSeenAt,
		next.RotatedAt, next.ForceRotate, old.TokenHash[:], old.ForceRotate)
	return tag.RowsAffected() == 1, err
}

// Expire rechecks the live row atomically so a stale snapshot cannot revoke a
// session that was just refreshed by another request. idleCutoff is now-idleTTL.
func (s *Sessions) Expire(ctx context.Context, id int64, now, idleCutoff time.Time) (bool, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE sessions s SET revoked_at = $2
		WHERE s.id = $1 AND s.revoked_at IS NULL AND
		(s.absolute_expires_at <= $2 OR s.last_seen_at <= $3 OR
		EXISTS (SELECT 1 FROM users u WHERE u.id = s.user_id AND NOT u.is_active))`, id, now, idleCutoff)
	return tag.RowsAffected() == 1, err
}

func (s *Sessions) Revoke(ctx context.Context, id int64, at time.Time) error {
	_, err := s.DB.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, at)
	return err
}

func (s *Sessions) MarkForceRotate(ctx context.Context, userID int64) error {
	_, err := s.DB.Exec(ctx,
		`UPDATE sessions SET force_rotate = true WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

func (s *Sessions) RevokeAllForUser(ctx context.Context, userID int64) error {
	_, err := s.DB.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}
