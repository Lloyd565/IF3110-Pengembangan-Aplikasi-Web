package workflow

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/batch"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/sqlpolicy"
)

func validateBatchRequest(x request) error {
	if x.invalidForwardMode {
		return migration.ValidationError("Pengajuan forward ini memakai mode normal untuk eksekusi batch. Ajukan forward baru dari riwayat.")
	}
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
	if x.Action != "rollback" {
		// Kompensasi batch hanya bisa mengembalikan baris yang tercatat di manifest, dan manifest hanya
		// direkam untuk UPDATE. DELETE/INSERT batch akan lolos jalan tapi tidak pernah bisa dikompensasi.
		if _, tracked, err := sqlpolicy.BatchReplay(x.SQL, x.CursorColumn, false); err != nil || !tracked {
			return migration.ValidationError("Mode batch hanya mendukung UPDATE, karena hanya baris UPDATE yang dicatat sehingga bisa dikompensasi.")
		}
	}
	if x.Action != "rollback" && strings.TrimSpace(x.downSQL) != "" {
		if err := batch.Validate(batch.Spec{RootID: 1, Op: batch.OpDown, SQL: x.downSQL, Size: x.BatchSize, Table: x.Scope, Column: x.CursorColumn}); err != nil {
			return migration.ValidationError("SQL down: " + err.Error())
		}
	}
	return nil
}

func (s *Service) validateTarget(ctx context.Context, q querier, connID uuid.UUID, x *request, inspectTarget bool) error {
	if x.invalidForwardMode {
		return validateBatchRequest(*x)
	}
	if strings.TrimSpace(x.SQL) != "" {
		if err := migration.CheckSQL(ctx, q, connID, x.SQL); err != nil {
			return err
		}
	}
	if !inspectTarget {
		return nil
	}
	var kind string
	if err := q.QueryRow(ctx, `SELECT dbms FROM db_connections WHERE id=$1 AND deleted_at IS NULL`, connID).Scan(&kind); err != nil {
		return err
	}
	if x.Mode == "batch" && kind != string(dbconn.PostgreSQL) {
		return migration.ValidationError("Cursor batch M2 tersedia untuk PostgreSQL; MariaDB memakai transaksi DML atau satu DDL per migration.")
	}
	if s.Targets != nil {
		var t *dbconn.Target
		var err error
		if registry, ok := s.Targets.(interface {
			TargetUsing(context.Context, dbconn.Querier, uuid.UUID) (*dbconn.Target, error)
		}); ok {
			t, err = registry.TargetUsing(ctx, q, connID)
		} else {
			t, err = s.Targets.Target(ctx, connID)
		}
		if err != nil {
			return err
		}
		if t.Identity != "" && t.Identity != x.identity {
			return conflict("Identitas target berubah.")
		}
		physical, err := t.PhysicalIdentity(ctx)
		if err != nil {
			return err
		}
		if x.physical != "" && x.physical != physical {
			return conflict("Server target berubah.")
		}
		x.physical = physical
		if x.Mode != "batch" {
			return nil
		}
		if err := (&batch.Runner{Pool: t.Postgres}).Validate(ctx, batch.Spec{RootID: 1, Op: batch.OpUp, SQL: x.SQL, Size: x.BatchSize, Table: x.Scope, Column: x.CursorColumn}); err != nil {
			if errors.Is(err, batch.ErrInvalidSpec) {
				return migration.ValidationError(err.Error())
			}
			return err
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

func (s *Service) AttemptStatus(ctx context.Context, id string, pl Plan) (string, error) {
	var status string
	err := s.DB.QueryRow(ctx, `SELECT e.status FROM migration_executions e JOIN db_connections dc ON dc.id=e.connection_id
  WHERE e.id=$1::uuid AND e.connection_id=$2 AND e.request_id=$3::uuid AND e.revision_id=$4::uuid AND e.sql=$5 AND e.action=$6
  AND e.target_identity <> '' AND e.target_identity=$7 AND e.target_server=$8 AND e.target_identity=`+dbconn.IdentityFingerprint+` AND dc.deleted_at IS NULL`,
		id, pl.ConnectionID, pl.RequestID, pl.RevisionID, pl.SQL, pl.Action, pl.TargetIdentity, pl.TargetServer).Scan(&status)
	return status, err
}

const targetFingerprint = dbconn.ConfigurationFingerprint
