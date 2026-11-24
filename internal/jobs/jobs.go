// Package jobs: queue persisten di Platform DB. Satu baris = satu eksekusi migration
// (apply, rollback, forward, resume, compensate). Worker mengklaim dengan lease; semua
// perubahan status memakai pagar worker_id supaya worker lama tidak bisa melanjutkan.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusRetryWait = "retry_wait"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	HoldNone              = "none"
	HoldPartial           = "partial"
	HoldPartiallyReverted = "partially_reverted"
	HoldCommitUnknown     = "commit_unknown"

	ModeNormal = "normal"
	ModeBatch  = "batch"
)

// ErrLeaseLost: worker ini sudah tidak memiliki job (lease habis dan diklaim worker lain).
var ErrLeaseLost = errors.New("jobs: kepemilikan job hilang")

// Querier dipenuhi *pgxpool.Pool dan pgx.Tx, supaya Enqueue bisa ikut transaksi platform.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type NewJob struct {
	ConnectionID   uuid.UUID
	RequestID      int64
	ExecutionID    int64
	IdempotencyKey string
	Operation      string
	SQL            string
	// Mode batch: SQL memakai $1 (batas bawah, eksklusif) dan $2 (batas atas, inklusif) pada BatchKey.
	Mode       string
	BatchSize  int
	BatchTable string
	BatchKey   string
	MaxRetries int
	TraceCtx   string
}

type Job struct {
	ID, RequestID, ExecutionID      int64
	ConnectionID                    uuid.UUID
	Operation, Mode, SQL            string
	BatchSize                       int
	BatchTable, BatchKey            string
	ScopeMax                        *int64
	Status, Hold                    string
	RetryCount, MaxRetries          int
	WorkerID                        string
	CommittedBatches, CommittedRows int64
	Error, TraceCtx                 string
	QueuedAt                        time.Time
	StartedAt                       *time.Time
}

const cols = `id, connection_id, request_id, execution_id, operation, mode, sql,
	coalesce(batch_size,0), coalesce(batch_table,''), coalesce(batch_key,''), batch_scope_max,
	status, hold, retry_count, max_retries, coalesce(worker_id,''), committed_batches, committed_rows,
	coalesce(error_message,''), trace_context, queued_at, started_at`

func scan(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.ConnectionID, &j.RequestID, &j.ExecutionID, &j.Operation, &j.Mode, &j.SQL,
		&j.BatchSize, &j.BatchTable, &j.BatchKey, &j.ScopeMax,
		&j.Status, &j.Hold, &j.RetryCount, &j.MaxRetries, &j.WorkerID, &j.CommittedBatches, &j.CommittedRows,
		&j.Error, &j.TraceCtx, &j.QueuedAt, &j.StartedAt)
	return &j, err
}

// Enqueue idempoten: kunci yang sama mengembalikan job lama (created=false), tidak membuat yang kedua.
func Enqueue(ctx context.Context, q Querier, in NewJob) (id int64, created bool, err error) {
	if in.Mode == "" {
		in.Mode = ModeNormal
	}
	if in.MaxRetries == 0 {
		in.MaxRetries = 3
	}
	var size, table, key any
	if in.Mode == ModeBatch {
		size, table, key = in.BatchSize, in.BatchTable, in.BatchKey
	}
	err = q.QueryRow(ctx, `
		INSERT INTO jobs (connection_id, request_id, execution_id, idempotency_key, operation, mode, sql,
		                  batch_size, batch_table, batch_key, max_retries, trace_context)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`,
		in.ConnectionID, in.RequestID, in.ExecutionID, in.IdempotencyKey, in.Operation, in.Mode, in.SQL,
		size, table, key, in.MaxRetries, in.TraceCtx).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	err = q.QueryRow(ctx, `SELECT id FROM jobs WHERE idempotency_key = $1`, in.IdempotencyKey).Scan(&id)
	return id, false, err
}

func Get(ctx context.Context, q Querier, id int64) (*Job, error) {
	return scan(q.QueryRow(ctx, `SELECT `+cols+` FROM jobs WHERE id = $1`, id))
}

// Claim mengambil satu job siap jalan: queued, retry_wait yang sudah waktunya, atau running
// dengan lease kedaluwarsa (worker sebelumnya mati). Tidak mengambil job pada database yang
// sedang dikerjakan job lain, dan job yang menahan database (hold) hanya boleh dilanjutkan
// oleh operasi pemulihan. Mengembalikan nil jika tidak ada pekerjaan.
func Claim(ctx context.Context, db *pgxpool.Pool, workerID string, lease time.Duration) (*Job, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var id int64
	var conn uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id, connection_id FROM jobs j
		WHERE (status = 'queued'
		       OR (status = 'retry_wait' AND next_retry_at <= now())
		       OR (status = 'running' AND lease_until < now()))
		  AND NOT EXISTS (SELECT 1 FROM jobs o WHERE o.connection_id = j.connection_id AND o.id <> j.id
		                  AND o.status = 'running' AND o.lease_until >= now())
		ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &conn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// SKIP LOCKED hanya menjaga baris job. Serialisasi per database dijaga di sini: dua worker
	// yang mengklaim job berbeda pada database yang sama bergantian lewat advisory lock ini.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('jobs.claim:' || $1::text))`, conn); err != nil {
		return nil, err
	}
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE connection_id = $1 AND id <> $2
		AND status = 'running' AND lease_until >= now())`, conn, id).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, nil
	}
	j, err := scan(tx.QueryRow(ctx, `
		UPDATE jobs SET status = 'running', worker_id = $2, lease_until = now() + $3::interval,
			started_at = coalesce(started_at, now()),
			retry_count = retry_count + (CASE WHEN status = 'running' THEN 1 ELSE 0 END)
		WHERE id = $1 RETURNING `+cols, id, workerID, lease.String()))
	if err != nil {
		return nil, err
	}
	return j, tx.Commit(ctx)
}

func fenced(ctx context.Context, q Querier, sql string, id int64, worker string, args ...any) error {
	tag, err := q.Exec(ctx, sql, append([]any{id, worker}, args...)...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Heartbeat memperpanjang lease. ErrLeaseLost berarti job sudah diambil worker lain.
func Heartbeat(ctx context.Context, q Querier, id int64, worker string, lease time.Duration) error {
	return fenced(ctx, q, `UPDATE jobs SET lease_until = now() + $3::interval
		WHERE id = $1 AND worker_id = $2 AND status = 'running'`, id, worker, lease.String())
}

// SetProgress menyinkronkan ringkasan progres dari bukti di target (idempoten: nilai absolut).
func SetProgress(ctx context.Context, q Querier, id int64, worker string, batches, rows int64, hold string) error {
	return fenced(ctx, q, `UPDATE jobs SET committed_batches = $3, committed_rows = $4, hold = $5
		WHERE id = $1 AND worker_id = $2 AND status = 'running'`, id, worker, batches, rows, hold)
}

func SetScope(ctx context.Context, q Querier, id int64, worker string, scopeMax int64) error {
	return fenced(ctx, q, `UPDATE jobs SET batch_scope_max = $3
		WHERE id = $1 AND worker_id = $2 AND status = 'running'`, id, worker, scopeMax)
}

func Succeed(ctx context.Context, q Querier, id int64, worker string) error {
	return fenced(ctx, q, `UPDATE jobs SET status = 'succeeded', hold = 'none', finished_at = now(),
		lease_until = NULL, error_message = NULL WHERE id = $1 AND worker_id = $2 AND status = 'running'`, id, worker)
}

// Fail mengakhiri job. hold menjelaskan keadaan database (partial, commit_unknown, ...).
func Fail(ctx context.Context, q Querier, id int64, worker, hold, msg string) error {
	return fenced(ctx, q, `UPDATE jobs SET status = 'failed', hold = $3, error_message = $4, finished_at = now(),
		lease_until = NULL WHERE id = $1 AND worker_id = $2 AND status = 'running'`, id, worker, hold, msg)
}

// Retry menjadwalkan ulang setelah jeda. Mengembalikan false jika jatah retry habis
// (job sudah ditandai failed dengan hold yang diberikan).
func Retry(ctx context.Context, q Querier, j *Job, worker, hold, msg string, backoff time.Duration) (bool, error) {
	if j.RetryCount >= j.MaxRetries {
		return false, Fail(ctx, q, j.ID, worker, hold, msg)
	}
	return true, fenced(ctx, q, `UPDATE jobs SET status = 'retry_wait', retry_count = retry_count + 1, hold = $3,
		error_message = $4, next_retry_at = now() + $5::interval, worker_id = NULL, lease_until = NULL
		WHERE id = $1 AND worker_id = $2 AND status = 'running'`, j.ID, worker, hold, msg, backoff.String())
}
