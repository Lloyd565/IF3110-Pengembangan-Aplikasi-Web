package migration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

var (
	LockTimeout      = 5 * time.Second
	StatementTimeout = 60 * time.Second

	ErrBusy              = errors.New("migration: migration lain sedang berjalan di database ini")
	ErrTargetUnavailable = errors.New("migration: database target tidak dapat dihubungi")
	ErrStatusPending     = errors.New("migration: status belum tersimpan di platform")

	errManualCheck = errors.New("migration: hasil di target tidak bisa dipastikan")
	errInterrupted = errors.New("Server berhenti saat migration berjalan; transaksi di Target DB tidak di-commit.")
)

const manualCheckMessage = "Server berhenti saat DDL MariaDB berjalan. DDL MariaDB tidak transaksional, jadi periksa Target DB secara manual: perubahan mungkin sudah atau belum diterapkan."

const (
	pgLock          = `SELECT pg_advisory_xact_lock(hashtext('if3110.migration'))`
	pgHistory       = `_platform.applied_migrations`
	pgCreateHistory = `CREATE SCHEMA IF NOT EXISTS _platform;
		CREATE TABLE IF NOT EXISTS _platform.applied_migrations (
			migration_id bigint PRIMARY KEY,
			applied_at   timestamptz NOT NULL DEFAULT now())`
)

type runner struct {
	validate func(sql string) error
	ping     func(context.Context) error
	run      func(ctx context.Context, id int64, sql string) error
	applied  func(ctx context.Context, id int64, sql string) (bool, error)
}

func pgRunner(target *pgxpool.Pool) runner {
	return runner{
		validate: CheckTransactional,
		ping:     target.Ping,
		run:      func(ctx context.Context, id int64, sql string) error { return runOnTarget(ctx, target, id, sql) },
		applied:  func(ctx context.Context, id int64, _ string) (bool, error) { return appliedPG(ctx, target, id) },
	}
}

var dollarTag = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z_0-9]*)?\$`)

func statements(sql string) []string {
	_, norm := split(sql)
	return norm
}

func split(sql string) (raw, norm []string) {
	var b strings.Builder
	start := 0
	emit := func(end int) {
		if st := strings.Join(strings.Fields(strings.ToLower(b.String())), " "); st != "" {
			raw = append(raw, strings.TrimSpace(sql[start:end]))
			norm = append(norm, st)
		}
		b.Reset()
		start = end + 1
	}
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
		case rest[0] == ';':
			emit(i)
			i++
		default:
			b.WriteByte(rest[0])
			i++
		}
	}
	emit(len(sql))
	return raw, norm
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
	return execute(ctx, platform, connID, id, userID, pgRunner(target))
}

func execute(ctx context.Context, platform *pgxpool.Pool, connID uuid.UUID, id, userID int64, r runner) error {
	m, err := Get(ctx, platform, connID, id)
	if err != nil {
		return err
	}
	if !m.IsDraft() {
		return ErrNotDraft
	}
	if err := r.validate(m.SQL); err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = r.ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTargetUnavailable, err)
	}

	sql, err := markRunningChecked(ctx, platform, connID, id, userID, r.validate)
	if err != nil {
		return err
	}

	runErr := r.run(ctx, id, sql)

	try := func(ctx context.Context) error { return settle(ctx, platform, r, id, sql, runErr) }
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := try(saveCtx); err != nil {
		log.Printf("migration %d: status gagal disimpan, dicoba ulang di background: %v", id, err)
		retryInBackground(id, try)
		return ErrStatusPending
	}
	return nil
}

func settle(ctx context.Context, platform *pgxpool.Pool, r runner, id int64, sql string, runErr error) error {
	status, msg := StatusSuccess, (*string)(nil)
	if runErr != nil {
		ok := false
		if !definiteFailure(runErr) {
			var err error
			ok, err = r.applied(ctx, id, sql)
			if errors.Is(err, errManualCheck) {
				runErr, err = errors.New(manualCheckMessage), nil
			}
			if err != nil {
				return err
			}
		}
		if !ok {
			status, msg = StatusFailed, ptr(errorMessage(runErr))
		}
	}
	_, err := platform.Exec(ctx, `
		UPDATE migrations SET status = $2, error_message = $3, finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'running'`, id, status, msg)
	return err
}

func definiteFailure(err error) bool {
	var pgErr *pgconn.PgError
	var myErr *mysql.MySQLError
	return errors.As(err, &pgErr) || errors.As(err, &myErr) || errors.As(err, new(ValidationError)) || errors.Is(err, ErrBusy)
}

func retryInBackground(id int64, try func(context.Context) error) {
	go func() {
		for wait := time.Second; wait <= 4*time.Minute; wait *= 2 {
			time.Sleep(wait)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := try(ctx)
			cancel()
			if err == nil {
				log.Printf("migration %d: status berhasil disinkronkan", id)
				return
			}
			log.Printf("migration %d: sinkronisasi status gagal: %v", id, err)
		}
	}()
}

func markRunning(ctx context.Context, platform *pgxpool.Pool, connID uuid.UUID, id, userID int64) (string, error) {
	return markRunningChecked(ctx, platform, connID, id, userID, CheckTransactional)
}

func markRunningChecked(ctx context.Context, platform *pgxpool.Pool, connID uuid.UUID, id, userID int64, validate func(string) error) (string, error) {
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
	if err := validate(sql); err != nil {
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

func runOnTarget(ctx context.Context, target *pgxpool.Pool, id int64, sql string) error {
	tx, err := target.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	setup := fmt.Sprintf(`SET LOCAL lock_timeout = %d; SET LOCAL statement_timeout = %d; %s`,
		LockTimeout.Milliseconds(), StatementTimeout.Milliseconds(), pgLock)
	if _, err := tx.Exec(ctx, setup); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pgHistory).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := tx.Exec(ctx, pgCreateHistory); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+pgHistory+` (migration_id) VALUES ($1)`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appliedPG(ctx context.Context, target *pgxpool.Pool, id int64) (bool, error) {
	tx, err := target.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// Ambil lock yang sama dengan migration: kalau transaksinya masih jalan, tunggu sampai selesai.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = %d; %s`, LockTimeout.Milliseconds(), pgLock)); err != nil {
		return false, err
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pgHistory).Scan(&ok); err != nil || !ok {
		return false, err
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgHistory+` WHERE migration_id = $1)`, id).Scan(&ok)
	return ok, err
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

func RecoverInterrupted(ctx context.Context, platform *pgxpool.Pool, open func(context.Context, uuid.UUID) (*dbconn.Target, error)) (int, error) {
	rows, err := platform.Query(ctx, `SELECT id, connection_id, sql FROM migrations WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	type pending struct {
		ID     int64
		ConnID uuid.UUID
		SQL    string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
	if err != nil {
		return 0, err
	}
	for _, p := range list {
		try := func(ctx context.Context) error {
			target, err := open(ctx, p.ConnID)
			if err != nil {
				return err
			}
			return settle(ctx, platform, targetRunner(target), p.ID, p.SQL, errInterrupted)
		}
		tryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := try(tryCtx)
		cancel()
		if err != nil {
			log.Printf("migration %d: pemulihan gagal, dicoba ulang di background: %v", p.ID, err)
			retryInBackground(p.ID, try)
		}
	}
	return len(list), nil
}
