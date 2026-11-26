package workflow

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/batch"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
)

func validateBatchRequest(x request) error {
	if x.Mode == "normal" {
		return nil
	}
	if x.Mode != "batch" {
		return migration.ValidationError("Mode eksekusi tidak valid.")
	}
	if x.BatchSize <= 0 {
		return migration.ValidationError("Ukuran batch harus positif.")
	}
	if err := batch.Validate(batch.Spec{RootID: 1, Op: batch.OpUp, SQL: x.SQL, Size: x.BatchSize, Table: x.Scope, Column: x.CursorColumn}); err != nil {
		return migration.ValidationError(err.Error())
	}
	if x.Action != "rollback" && strings.TrimSpace(x.downSQL) != "" {
		if err := batch.Validate(batch.Spec{RootID: 1, Op: batch.OpDown, SQL: x.downSQL, Size: x.BatchSize, Table: x.Scope, Column: x.CursorColumn}); err != nil {
			return migration.ValidationError("SQL down: " + err.Error())
		}
	}
	return nil
}

func (s *Service) validateTarget(ctx context.Context, connID uuid.UUID, x request) error {
	if strings.TrimSpace(x.SQL) != "" {
		if err := migration.CheckSQL(ctx, s.DB, connID, x.SQL); err != nil {
			return err
		}
	}
	if x.Mode != "batch" {
		return nil
	}
	var kind string
	if err := s.DB.QueryRow(ctx, `SELECT dbms FROM db_connections WHERE id=$1 AND deleted_at IS NULL`, connID).Scan(&kind); err != nil {
		return err
	}
	if kind != string(dbconn.PostgreSQL) {
		return migration.ValidationError("Cursor batch M2 tersedia untuk PostgreSQL; MariaDB memakai transaksi DML atau satu DDL per migration.")
	}
	if s.Targets != nil {
		t, err := s.Targets.Target(ctx, connID)
		if err != nil {
			return err
		}
		if err := (&batch.Runner{Pool: t.Postgres}).Validate(ctx, batch.Spec{RootID: 1, Op: batch.OpUp, SQL: x.SQL, Size: x.BatchSize, Table: x.Scope, Column: x.CursorColumn}); err != nil {
			return migration.ValidationError(err.Error())
		}
	}
	return nil
}

func (s *Service) BeginAttemptAtomic(ctx context.Context, pl Plan, jobID string, no int, attach func(pgx.Tx, string) error) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		id, err = beginAttempt(ctx, tx, pl, jobID, no)
		if err != nil {
			return err
		}
		return attach(tx, id)
	})
	return id, err
}

func (s *Service) FinishAttemptAtomic(ctx context.Context, executionID, status, msg string, finish func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := finishAttempt(ctx, tx, executionID, status, msg); err != nil {
			return err
		}
		return finish(tx)
	})
}

func (s *Service) AttemptStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := s.DB.QueryRow(ctx, `SELECT status FROM migration_executions WHERE id=$1::uuid`, id).Scan(&status)
	return status, err
}

const targetFingerprint = `encode(sha256(convert_to(jsonb_build_array(dc.dbms,dc.host,dc.port,dc.dbname,dc.username,encode(dc.password_enc,'hex'),dc.sslmode,dc.deleted_at)::text,'UTF8')),'hex')`
