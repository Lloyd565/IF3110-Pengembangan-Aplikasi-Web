package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/batch"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

const mariaRuns = "_platform_workflow_runs"

func mariaEvidence(ctx context.Context, conn *sql.Conn, root int64, op string) (batch.Progress, bool, error) {
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?`, mariaRuns).Scan(&exists); err != nil || exists == 0 {
		return batch.Progress{}, false, err
	}
	var status string
	var rows int64
	err := conn.QueryRowContext(ctx, `SELECT status, rows_affected FROM `+mariaRuns+` WHERE root_job=? AND op=?`, root, op).Scan(&status, &rows)
	if errors.Is(err, sql.ErrNoRows) {
		return batch.Progress{}, false, nil
	}
	if err != nil {
		return batch.Progress{}, false, err
	}
	if status != "success" {
		return batch.Progress{}, false, fmt.Errorf("%w: DDL MariaDB memerlukan rekonsiliasi manual sebelum dicoba kembali", batch.ErrCommitUnknown)
	}
	return batch.Progress{Batches: 1, Rows: rows, Total: &rows}, true, nil
}

func WorkflowEvidence(ctx context.Context, target *dbconn.Target, root int64, op, source string) (batch.Progress, bool, error) {
	var progress batch.Progress
	var done bool
	err := withMaria(ctx, target, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		progress, done, err = mariaEvidence(ctx, conn, root, op)
		return err
	})
	return progress, done, err
}

func RunWorkflowMaria(ctx context.Context, target *dbconn.Target, root int64, op, source string, revalidate func(context.Context) error) (batch.Progress, error) {
	stmts, ddl, err := mariaSQL(source)
	if err != nil {
		return batch.Progress{}, err
	}
	if err := mariaScope(stmts, target.Database); err != nil {
		return batch.Progress{}, err
	}
	var progress batch.Progress
	err = withMaria(ctx, target, func(ctx context.Context, conn *sql.Conn) error {
		p, done, err := mariaEvidence(ctx, conn, root, op)
		if err != nil || done {
			progress = p
			return err
		}
		if revalidate != nil {
			if err := revalidate(ctx); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+mariaRuns+` (
			root_job BIGINT NOT NULL, op VARCHAR(8) NOT NULL, status VARCHAR(16) NOT NULL,
			rows_affected BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(root_job,op)) ENGINE=InnoDB`); err != nil {
			return err
		}
		if ddl {
			if _, err := conn.ExecContext(ctx, `INSERT INTO `+mariaRuns+` (root_job,op,status) VALUES (?,?,'running')`, root, op); err != nil {
				return err
			}
			result, err := conn.ExecContext(ctx, stmts[0].SQL)
			if err != nil {
				var definite *mysql.MySQLError
				if errors.As(err, &definite) {
					if _, cleanupErr := conn.ExecContext(ctx, `DELETE FROM `+mariaRuns+` WHERE root_job=? AND op=?`, root, op); cleanupErr == nil {
						return err
					}
				}
				return batch.ErrCommitUnknown
			}
			n, _ := result.RowsAffected()
			if _, err := conn.ExecContext(ctx, `UPDATE `+mariaRuns+` SET status='success',rows_affected=? WHERE root_job=? AND op=?`, n, root, op); err != nil {
				return batch.ErrCommitUnknown
			}
			progress = batch.Progress{Batches: 1, Rows: n, Total: &n}
			return nil
		}
		if err := requireInnoDB(ctx, conn, target.Database); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var n int64
		for _, st := range stmts {
			tag, err := tx.ExecContext(ctx, st.SQL)
			if err != nil {
				return err
			}
			n, _ = tag.RowsAffected()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+mariaRuns+` (root_job,op,status,rows_affected) VALUES (?,?,'success',?)`, root, op, n); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return batch.ErrCommitUnknown
		}
		progress = batch.Progress{Batches: 1, Rows: n, Total: &n}
		return nil
	})
	return progress, err
}
