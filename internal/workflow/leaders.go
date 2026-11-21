package workflow

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

type Leaders struct{ DB *pgxpool.Pool }

var _ contracts.Leaders = (*Leaders)(nil)

func (l *Leaders) List(ctx context.Context, _ reqctx.Principal) ([]contracts.Leader, error) {
	rows, err := l.DB.Query(ctx, `
		SELECT gl.group_id, gl.user_id, u.username::text
		FROM group_leaders gl JOIN users u ON u.id = gl.user_id ORDER BY gl.group_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[contracts.Leader])
}

func (l *Leaders) Set(ctx context.Context, p reqctx.Principal, groupID, userID int64) error {
	var active bool
	err := l.DB.QueryRow(ctx, `SELECT is_active FROM users WHERE id = $1`, userID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !active {
		return &contracts.Error{Status: 422, Code: "validation", Message: "Leader harus pengguna aktif."}
	}
	_, err = l.DB.Exec(ctx, `
		INSERT INTO group_leaders (group_id, user_id) VALUES ($1, $2)
		ON CONFLICT (group_id) DO UPDATE SET user_id = EXCLUDED.user_id, updated_at = now()`, groupID, userID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return &contracts.Error{Status: 422, Code: "validation", Message: "Leader harus anggota group tersebut."}
	}
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "group.leader_set", "actor", p.UserID, "group_id", groupID, "leader_id", userID)
	return nil
}
