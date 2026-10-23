// Package dbconn: pool per koneksi target (owner: B).
package dbconn

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Registry struct {
	DB *pgxpool.Pool // platform DB, untuk membaca db_connections

	devOnce sync.Once
	devPool *pgxpool.Pool
	devErr  error
}

// Pool mengembalikan pool ke Target DB untuk koneksi connID. Jangan di-Close oleh pemanggil.
// TODO(B): STUB sementara. Semua connID diarahkan ke DEV_TARGET_DATABASE_URL
// supaya C bisa jalan tanpa menunggu B. Ganti dengan baca db_connections + decrypt.
func (r *Registry) Pool(ctx context.Context, connID uuid.UUID) (*pgxpool.Pool, error) {
	r.devOnce.Do(func() {
		url := os.Getenv("DEV_TARGET_DATABASE_URL")
		if url == "" {
			r.devErr = errors.New("dbconn: belum diimplementasi (set DEV_TARGET_DATABASE_URL untuk dev)")
			return
		}
		r.devPool, r.devErr = pgxpool.New(context.Background(), url)
	})
	return r.devPool, r.devErr
}
