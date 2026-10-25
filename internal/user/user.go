package user

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUsernameTaken = errors.New("user: username sudah dipakai")
	ErrNotFound      = errors.New("user: tidak ditemukan")
)

type User struct {
	ID              int64
	Username        string
	IsPlatformAdmin bool
	IsActive        bool
	CreatedAt       time.Time
}

type Repo struct {
	DB *pgxpool.Pool
}

func (r *Repo) Create(ctx context.Context, username, passwordHash string, admin bool) (int64, error) {
	var id int64
	err := r.DB.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, is_platform_admin) VALUES ($1, $2, $3) RETURNING id`,
		username, passwordHash, admin).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, ErrUsernameTaken
	}
	return id, err
}

func (r *Repo) List(ctx context.Context) ([]User, error) {
	rows, err := r.DB.Query(ctx,
		`SELECT id, username, is_platform_admin, is_active, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsPlatformAdmin, &u.IsActive, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *Repo) update(ctx context.Context, sql string, args ...any) error {
	tag, err := r.DB.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) SetActive(ctx context.Context, id int64, active bool) error {
	return r.update(ctx, `UPDATE users SET is_active = $2, updated_at = now() WHERE id = $1`, id, active)
}

func (r *Repo) SetAdmin(ctx context.Context, id int64, admin bool) error {
	return r.update(ctx, `UPDATE users SET is_platform_admin = $2, updated_at = now() WHERE id = $1`, id, admin)
}

func (r *Repo) SetPasswordHash(ctx context.Context, id int64, hash string) error {
	return r.update(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, hash)
}

// EnsureAdmin: bootstrap admin pertama. Hanya membuat akun jika belum ada
// satu pun platform admin; kalau sudah ada, tidak melakukan apa-apa.
func (r *Repo) EnsureAdmin(ctx context.Context, username, passwordHash string) (created bool, err error) {
	tag, err := r.DB.Exec(ctx, `
		INSERT INTO users (username, password_hash, is_platform_admin)
		SELECT $1, $2, true
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE is_platform_admin)
		ON CONFLICT (username) DO NOTHING`, username, passwordHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
