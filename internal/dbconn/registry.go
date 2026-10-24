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
	DB *pgxpool.Pool

	devOnce sync.Once
	devPool *pgxpool.Pool
	devErr  error
}

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
