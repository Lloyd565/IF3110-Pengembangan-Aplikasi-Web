// Package auth: session & password (owner: A).
package auth

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Sessions struct {
	DB *pgxpool.Pool
}

// MarkForceRotate: dipanggil B saat privilege user naik (grant bertambah / jadi admin).
// Session aktif user dirotasi pada request berikutnya.
func (s *Sessions) MarkForceRotate(ctx context.Context, userID int64) error {
	_, err := s.DB.Exec(ctx,
		`UPDATE sessions SET force_rotate = true WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// RevokeAllForUser: dipanggil B saat user dinonaktifkan atau password di-reset.
func (s *Sessions) RevokeAllForUser(ctx context.Context, userID int64) error {
	_, err := s.DB.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}
