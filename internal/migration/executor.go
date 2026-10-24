package migration

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	LockTimeout      = 5 * time.Second
	StatementTimeout = 60 * time.Second

	ErrBusy              = errors.New("migration: migration lain sedang berjalan di database ini")
	ErrTargetUnavailable = errors.New("migration: database target tidak dapat dihubungi")
)

const interruptedMessage = "Server berhenti saat migration berjalan. Periksa Target DB secara manual: perubahan mungkin sudah atau belum diterapkan."

var dollarTag = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z_0-9]*)?\$`)

func statements(sql string) []string {
	var b strings.Builder
	for i := 0; i < len(sql); {
		rest := sql[i:]
		switch {
		case strings.HasPrefix(rest, "--"):
			if j := strings.IndexByte(rest, '\n'); j >= 0 {
				i += j
			} else {
				i = len(sql)
			}
		case strings.HasPrefix(rest, "/*"):
			if j := strings.Index(rest[2:], "*/"); j >= 0 {
				i += j + 4
			} else {
				i = len(sql)
			}
			b.WriteByte(' ')
		case rest[0] == '\'' || rest[0] == '"':
			q, j := rest[0], 1
			for j < len(rest) {
				if rest[j] == q {
					if j+1 < len(rest) && rest[j+1] == q {
						j += 2
						continue
					}
					break
				}
				j++
			}
			i += j + 1
			b.WriteString(" x ")
		case rest[0] == '$' && dollarTag.MatchString(rest):
			tag := dollarTag.FindString(rest)
			if j := strings.Index(rest[len(tag):], tag); j >= 0 {
				i += len(tag) + j + len(tag)
			} else {
				i = len(sql)
			}
			b.WriteString(" x ")
		default:
			b.WriteByte(rest[0])
			i++
		}
	}

	var out []string
	for st := range strings.SplitSeq(b.String(), ";") {
		if st = strings.Join(strings.Fields(strings.ToLower(st)), " "); st != "" {
			out = append(out, st)
		}
	}
	return out
}

var (
	txControl       = []string{"begin", "start transaction", "commit", "end", "rollback", "abort", "prepare transaction"}
	nonTransactable = []string{"vacuum", "create database", "drop database", "alter system", "create tablespace", "drop tablespace", "reindex database", "reindex system"}
)

func hasPrefixWord(st, p string) bool { return st == p || strings.HasPrefix(st, p+" ") }

func CheckTransactional(sql string) error {
	stmts := statements(sql)
	if len(stmts) == 0 {
		return ValidationError("SQL migration tidak berisi perintah apa pun.")
	}
	for _, st := range stmts {
		for _, p := range txControl {
			if hasPrefixWord(st, p) {
				return ValidationError(fmt.Sprintf("Migration tidak boleh berisi %s. Platform sudah menjalankan seluruh SQL dalam satu transaksi.", strings.ToUpper(p)))
			}
		}
		for _, p := range nonTransactable {
			if hasPrefixWord(st, p) {
				return ValidationError(fmt.Sprintf("Perintah %s tidak bisa dijalankan di dalam transaksi, jadi tidak bisa lewat migration.", strings.ToUpper(p)))
			}
		}
		if strings.Contains(" "+st+" ", " concurrently ") && !hasPrefixWord(st, "refresh materialized view") {
			return ValidationError("Perintah dengan CONCURRENTLY tidak bisa dijalankan di dalam transaksi. Hapus kata CONCURRENTLY.")
		}
	}
	return nil
}

func Execute(ctx context.Context, platform, target *pgxpool.Pool, connID uuid.UUID, id, userID int64) error {
	m, err := Get(ctx, platform, connID, id)
	if err != nil {
		return err
	}
	if !m.IsDraft() {
		return ErrNotDraft
	}
	if err := CheckTransactional(m.SQL); err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = target.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTargetUnavailable, err)
	}

	sql, err := markRunning(ctx, platform, connID, id, userID)
	if err != nil {
		return err
	}

	runErr := runOnTarget(ctx, target, sql)

	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	status, msg := StatusSuccess, (*string)(nil)
	if runErr != nil {
		status, msg = StatusFailed, ptr(errorMessage(runErr))
	}
	_, err = platform.Exec(saveCtx, `
		UPDATE migrations SET status = $2, error_message = $3, finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'running'`, id, status, msg)
	return err
}

func markRunning(ctx context.Context, platform *pgxpool.Pool, connID uuid.UUID, id, userID int64) (string, error) {
	tx, err := platform.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT 1 FROM db_connections WHERE id = $1 FOR UPDATE`, connID); err != nil {
		return "", err
	}
	var status, sql string
	err = tx.QueryRow(ctx, `SELECT status, sql FROM migrations WHERE connection_id = $1 AND id = $2 FOR UPDATE`, connID, id).Scan(&status, &sql)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if status != StatusDraft {
		return "", ErrNotDraft
	}
	if err := CheckTransactional(sql); err != nil {
		return "", err
	}
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM migrations WHERE connection_id = $1 AND status = 'running')`, connID).Scan(&busy); err != nil {
		return "", err
	}
	if busy {
		return "", ErrBusy
	}
	if _, err := tx.Exec(ctx, `
		UPDATE migrations SET
			status = 'running',
			version = (SELECT coalesce(max(version), 0) + 1 FROM migrations WHERE connection_id = $1),
			executed_by = $3, started_at = now(), updated_at = now()
		WHERE id = $2`, connID, id, userID); err != nil {
		return "", err
	}
	return sql, tx.Commit(ctx)
}

func runOnTarget(ctx context.Context, target *pgxpool.Pool, sql string) error {
	tx, err := target.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	setup := fmt.Sprintf(`SET LOCAL lock_timeout = %d; SET LOCAL statement_timeout = %d; SELECT pg_advisory_xact_lock(hashtext('if3110.migration'))`,
		LockTimeout.Milliseconds(), StatementTimeout.Milliseconds())
	if _, err := tx.Exec(ctx, setup); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func errorMessage(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err.Error()
	}
	msg := fmt.Sprintf("%s: %s (SQLSTATE %s)", pgErr.Severity, pgErr.Message, pgErr.Code)
	if pgErr.Detail != "" {
		msg += "\nDetail: " + pgErr.Detail
	}
	if pgErr.Hint != "" {
		msg += "\nHint: " + pgErr.Hint
	}
	if pgErr.Where != "" {
		msg += "\nKonteks: " + pgErr.Where
	}
	return msg
}

func ptr[T any](v T) *T { return &v }

func RecoverInterrupted(ctx context.Context, platform *pgxpool.Pool) (int64, error) {
	tag, err := platform.Exec(ctx, `
		UPDATE migrations SET status = 'failed', error_message = $1, finished_at = now(), updated_at = now()
		WHERE status = 'running'`, interruptedMessage)
	return tag.RowsAffected(), err
}
