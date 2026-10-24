package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

var ErrCredentials = errors.New("username atau password salah")

type Credentials struct {
	DB    *pgxpool.Pool
	dummy string
}

func NewCredentials(db *pgxpool.Pool) (*Credentials, error) {
	random, err := NewToken()
	if err != nil {
		return nil, err
	}
	dummy, err := HashPassword(random)
	if err != nil {
		return nil, err
	}
	return &Credentials{DB: db, dummy: dummy}, nil
}

func (c *Credentials) Authenticate(ctx context.Context, username, password string) (reqctx.Principal, error) {
	var p reqctx.Principal
	var hash string
	var active bool
	err := c.DB.QueryRow(ctx, `SELECT id, username, password_hash, is_platform_admin, is_active
		FROM users WHERE username = $1`, strings.TrimSpace(username)).Scan(&p.UserID, &p.Username, &hash, &p.IsPlatformAdmin, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		VerifyPassword(c.dummy, password)
		return reqctx.Principal{}, ErrCredentials
	}
	if err != nil {
		return reqctx.Principal{}, err
	}
	valid := VerifyPassword(hash, password)
	if !valid || !active {
		return reqctx.Principal{}, ErrCredentials
	}
	return p, nil
}
