package migration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/go-sql-driver/mysql"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/batch"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

const mariaRuns = "_platform_workflow_runs"

func verifyMariaServer(ctx context.Context, conn *sql.Conn, server string) error {
	if server == "" {
		return nil
	}
	actual, err := dbconn.MariaIdentity(ctx, conn)
	if err != nil {
		return err
	}
	if actual != server {
		return dbconn.ErrTargetIdentity
	}
	return nil
}

type mariaRun struct {
	status string
	rows   int64
}

func mariaProof(target *dbconn.Target, binding string, root int64, op string, r mariaRun) []byte {
	mac := hmac.New(sha256.New, target.EvidenceKey)
	fmt.Fprintf(mac, "maria-v1\x00%s\x00%d\x00%s\x00%s\x00%d", binding, root, op, r.status, r.rows)
	return mac.Sum(nil)
}

func mariaEvidence(ctx context.Context, conn *sql.Conn, target *dbconn.Target, binding string, root int64, op string, allowNew bool) (mariaRun, error) {
	var safe bool
	err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.TABLES
  WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND TABLE_TYPE='BASE TABLE' AND ENGINE='InnoDB')
  AND NOT EXISTS(SELECT 1 FROM information_schema.TRIGGERS WHERE EVENT_OBJECT_SCHEMA=DATABASE() AND EVENT_OBJECT_TABLE=?)`, mariaRuns, mariaRuns).Scan(&safe)
	if err != nil {
		return mariaRun{}, err
	}
	if !safe {
		return mariaRun{}, batch.ErrCommitUnknown
	}
	var r mariaRun
	var proof []byte
	err = conn.QueryRowContext(ctx, `SELECT status,rows_affected,proof FROM `+mariaRuns+` WHERE root_job=? AND op=?`, root, op).Scan(&r.status, &r.rows, &proof)
	if errors.Is(err, sql.ErrNoRows) && allowNew {
		r.status = "ready"
		_, err = conn.ExecContext(ctx, `INSERT INTO `+mariaRuns+`(root_job,op,status,proof) VALUES(?,?,?,?)`, root, op, r.status, mariaProof(target, binding, root, op, r))
		return r, err
	}
	if err != nil {
		var server *mysql.MySQLError
		if errors.Is(err, sql.ErrNoRows) || (errors.As(err, &server) && (server.Number == 1146 || server.Number == 1054)) {
			return r, batch.ErrCommitUnknown
		}
		return r, err
	}
	if !hmac.Equal(proof, mariaProof(target, binding, root, op, r)) || (r.status != "ready" && r.status != "success") {
		return r, batch.ErrCommitUnknown
	}
	return r, nil
}

func WorkflowEvidence(ctx context.Context, target *dbconn.Target, root int64, op, binding, server string) (batch.Progress, bool, error) {
	var r mariaRun
	err := withMaria(ctx, target, func(ctx context.Context, conn *sql.Conn) error {
		if err := verifyMariaServer(ctx, conn, server); err != nil {
			return err
		}
		var err error
		r, err = mariaEvidence(ctx, conn, target, binding, root, op, false)
		return err
	})
	if err != nil || r.status != "success" {
		return batch.Progress{}, false, err
	}
	return batch.Progress{Batches: 1, Rows: r.rows, Total: &r.rows}, true, nil
}

type mariaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func storeMariaRun(ctx context.Context, q mariaExecer, target *dbconn.Target, binding string, root int64, op string, r mariaRun) error {
	tag, err := q.ExecContext(ctx, `UPDATE `+mariaRuns+` SET status=?,rows_affected=?,proof=? WHERE root_job=? AND op=?`,
		r.status, r.rows, mariaProof(target, binding, root, op, r), root, op)
	if err != nil {
		return err
	}
	if n, err := tag.RowsAffected(); err != nil || n != 1 {
		return batch.ErrCommitUnknown
	}
	return nil
}

type mariaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func workflowMariaState(ctx context.Context, q mariaQueryer) ([]mariaCheckpoint, error) {
	rows, err := q.QueryContext(ctx, `SELECT root_job,op,status,rows_affected,coalesce(hex(proof),'') FROM `+mariaRuns+` ORDER BY root_job,op`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var state []mariaCheckpoint
	for rows.Next() {
		var r mariaCheckpoint
		if err := rows.Scan(&r.root, &r.op, &r.status, &r.rows, &r.proof); err != nil {
			return nil, err
		}
		state = append(state, r)
	}
	return state, rows.Err()
}

type mariaCheckpoint struct {
	root, rows        int64
	op, status, proof string
}

func RunWorkflowMaria(ctx context.Context, target *dbconn.Target, root int64, op, source string, revalidate, arm func(context.Context) error, binding, server string) (batch.Progress, error) {
	stmts, ddl, err := mariaSQL(source)
	if err != nil {
		return batch.Progress{}, err
	}
	if err := mariaScope(stmts, target.Database); err != nil {
		return batch.Progress{}, err
	}
	var progress batch.Progress
	err = withMaria(ctx, target, func(ctx context.Context, conn *sql.Conn) error {
		if err := verifyMariaServer(ctx, conn, server); err != nil {
			return err
		}
		if revalidate != nil {
			if err := revalidate(ctx); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+mariaRuns+` (
   root_job BIGINT NOT NULL,op VARCHAR(8) NOT NULL,status VARCHAR(16) NOT NULL,
   rows_affected BIGINT NOT NULL DEFAULT 0,proof VARBINARY(32),PRIMARY KEY(root_job,op)) ENGINE=InnoDB`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `ALTER TABLE `+mariaRuns+` ADD COLUMN IF NOT EXISTS proof VARBINARY(32)`); err != nil {
			return err
		}
		r, err := mariaEvidence(ctx, conn, target, binding, root, op, true)
		if err != nil {
			return err
		}
		if r.status == "success" {
			progress = batch.Progress{Batches: 1, Rows: r.rows, Total: &r.rows}
			return nil
		}
		if arm != nil {
			if err := arm(ctx); err != nil {
				return err
			}
		}
		if ddl {
			if err := storeMariaRun(ctx, conn, target, binding, root, op, mariaRun{status: "running"}); err != nil {
				return err
			}
			before, err := workflowMariaState(ctx, conn)
			if err != nil {
				return batch.ErrCommitUnknown
			}
			result, err := conn.ExecContext(ctx, stmts[0].SQL)
			if err != nil {
				var definite *mysql.MySQLError
				if errors.As(err, &definite) && storeMariaRun(ctx, conn, target, binding, root, op, mariaRun{status: "ready"}) == nil {
					return err
				}
				return batch.ErrCommitUnknown
			}
			after, err := workflowMariaState(ctx, conn)
			if err != nil || !slices.Equal(before, after) || verifyMariaServer(ctx, conn, server) != nil {
				return batch.ErrCommitUnknown
			}
			r.rows, _ = result.RowsAffected()
			r.status = "success"
			if err := storeMariaRun(ctx, conn, target, binding, root, op, r); err != nil {
				return batch.ErrCommitUnknown
			}
		} else {
			if err := requireInnoDB(ctx, conn, target.Database); err != nil {
				return err
			}
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			before, err := workflowMariaState(ctx, tx)
			if err != nil {
				return err
			}
			for _, st := range stmts {
				tag, err := tx.ExecContext(ctx, st.SQL)
				if err != nil {
					return err
				}
				r.rows, _ = tag.RowsAffected()
			}
			after, err := workflowMariaState(ctx, tx)
			if err != nil {
				return err
			}
			if !slices.Equal(before, after) {
				return batch.ErrUnsafe
			}
			var actual string
			if server != "" {
				if err := tx.QueryRowContext(ctx, `SELECT CONCAT('maria:',@@server_uid,':',DATABASE(),':',identity_token) FROM `+dbconn.MariaIdentityTable+` WHERE id=1`).Scan(&actual); err != nil || actual != server {
					return batch.ErrUnsafe
				}
			}
			r.status = "success"
			if err := storeMariaRun(ctx, tx, target, binding, root, op, r); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return batch.ErrCommitUnknown
			}
		}
		progress = batch.Progress{Batches: 1, Rows: r.rows, Total: &r.rows}
		return nil
	})
	return progress, err
}
