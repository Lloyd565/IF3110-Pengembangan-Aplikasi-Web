package auth

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Sessions struct {
	DB *pgxpool.Pool
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
