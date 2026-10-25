package migration

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateDraftBatch persists all generated drafts atomically without running any
// target SQL. Every draft still passes the ordinary dialect validation.
func CreateDraftBatch(ctx context.Context, db *pgxpool.Pool, connID uuid.UUID, userID int64, name string, statements []string) ([]int64, error) {
	if len(statements) == 0 || len(statements) > 250 {
		return nil, ValidationError("Batch harus berisi 1 sampai 250 draft.")
	}
	type draft struct{ name, sql string }
	drafts := make([]draft, len(statements))
	for i, q := range statements {
		draftName := name
		if len(statements) > 1 {
			draftName = fmt.Sprintf("[%03d/%03d] %s", i+1, len(statements), name)
		}
		n, q, err := validateConnection(ctx, db, connID, draftName, q)
		if err != nil {
			return nil, err
		}
		drafts[i] = draft{n, q}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tx.Rollback(cleanup)
	}()
	var ids []int64
	for _, d := range drafts {
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO migrations(connection_id,name,sql,created_by) VALUES($1,$2,$3,$4) RETURNING id`, connID, d.name, d.sql, userID).Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}
