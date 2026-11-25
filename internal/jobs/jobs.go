// Package jobs: queue persisten di Platform DB. Satu baris = satu pekerjaan worker.
// Worker mengklaim dengan lease; semua perubahan status dipagari worker_id supaya
// worker lama tidak bisa melanjutkan setelah kepemilikan berpindah.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusRetryWait = "retry_wait"
	StatusSucceeded = "success"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	HoldNone              = "none"
	HoldPartial           = "partial"
	HoldPartiallyReverted = "partially_reverted"
	HoldCommitUnknown     = "commit_unknown"

	KindRun        = "run"
	KindResume     = "resume"
	KindCompensate = "compensate"
)

// ErrLeaseLost: worker ini sudah tidak memiliki job (lease habis dan diklaim worker lain).
var ErrLeaseLost = errors.New("jobs: kepemilikan job hilang")

// Querier dipenuhi *pgxpool.Pool dan pgx.Tx, supaya Enqueue bisa ikut transaksi platform.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type NewJob struct {
	ConnectionID uuid.UUID
	RequestID    uuid.UUID
	Key          string
	Kind         string
	ParentJobID  *int64
	Plan         json.RawMessage
	MaxRetries   int
	TraceCtx     string
}

type Job struct {
	ID               int64
	ConnectionID     uuid.UUID
	RequestID        uuid.UUID
	Kind             string
	ParentID         *int64
	RootID           int64
	Plan             json.RawMessage
	Status, Hold     string
	Attempt          int
	RetryCount       int
	MaxRetries       int
	WorkerID         string
	ExecutionID      string
	AttemptOpen      bool // attempt history sudah dibuka tetapi hasilnya belum dicatat
	CommittedBatches int64
	CommittedRows    int64
	TotalRows        *int64
	Error, TraceCtx  string
	QueuedAt         time.Time
	StartedAt        *time.Time
	UpdatedAt        time.Time
}

// Terminal berarti worker tidak akan menyentuh job ini lagi.
func (j *Job) Terminal() bool {
	return j.Status == StatusSucceeded || j.Status == StatusFailed || j.Status == StatusCancelled
}

// PublicStatus: nama status untuk API/SPA. Job gagal yang meninggalkan perubahan committed
// dilaporkan sebagai hold-nya (partial, partially_reverted, commit_unknown); retry_wait tampil queued.
func (j *Job) PublicStatus() string {
	switch {
	case j.Status == StatusFailed && j.Hold != HoldNone:
		return j.Hold
	case j.Status == StatusRetryWait:
		return StatusQueued
	}
	return j.Status
}

const cols = `id, connection_id, request_id, kind, parent_job_id, root_job_id, plan, status, hold, attempt,
	retry_count, max_retries, coalesce(worker_id,''), coalesce(execution_id::text,''), attempt_open, committed_batches,
	committed_rows, total_rows, coalesce(error_message,''), trace_context, queued_at, started_at, updated_at`

func scan(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.ConnectionID, &j.RequestID, &j.Kind, &j.ParentID, &j.RootID, &j.Plan, &j.Status,
		&j.Hold, &j.Attempt, &j.RetryCount, &j.MaxRetries, &j.WorkerID, &j.ExecutionID, &j.AttemptOpen, &j.CommittedBatches,
		&j.CommittedRows, &j.TotalRows, &j.Error, &j.TraceCtx, &j.QueuedAt, &j.StartedAt, &j.UpdatedAt)
	return &j, err
}

// Enqueue idempoten: kunci yang sama mengembalikan job lama (created=false), tidak membuat yang kedua.
func Enqueue(ctx context.Context, q Querier, in NewJob) (id int64, created bool, err error) {
	if in.Kind == "" {
		in.Kind = KindRun
	}
	if in.MaxRetries == 0 {
		in.MaxRetries = 3
	}
	var root *int64
	var batches, rows int64
	var total *int64
	hold := HoldNone
	if in.ParentJobID != nil {
		parent, err := Get(ctx, q, *in.ParentJobID)
		if err != nil {
			return 0, false, err
		}
		root = &parent.RootID
		if in.Kind == KindResume {
			batches, rows, total, hold = parent.CommittedBatches, parent.CommittedRows, parent.TotalRows, parent.Hold
		}
	}
	err = q.QueryRow(ctx, `
		WITH n AS (SELECT nextval(pg_get_serial_sequence('jobs', 'id')) AS id)
		INSERT INTO jobs (id, connection_id, request_id, idempotency_key, kind, parent_job_id, root_job_id, plan,
		                  max_retries, trace_context, committed_batches, committed_rows, total_rows, hold)
		SELECT n.id, $1, $2, $3, $4, $5, coalesce($6::bigint, n.id), $7, $8, $9, $10, $11, $12, $13 FROM n
		ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`,
		in.ConnectionID, in.RequestID, in.Key, in.Kind, in.ParentJobID, root, in.Plan, in.MaxRetries, in.TraceCtx,
		batches, rows, total, hold).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	err = q.QueryRow(ctx, `SELECT id FROM jobs WHERE idempotency_key = $1`, in.Key).Scan(&id)
	return id, false, err
}

func Get(ctx context.Context, q Querier, id int64) (*Job, error) {
	return scan(q.QueryRow(ctx, `SELECT `+cols+` FROM jobs WHERE id = $1`, id))
}

func List(ctx context.Context, q Querier, connID uuid.UUID, limit int) ([]*Job, error) {
	rows, err := q.Query(ctx, `SELECT `+cols+` FROM jobs WHERE connection_id = $1 ORDER BY id DESC LIMIT $2`, connID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Job, error) { return scan(r) })
}

// Claim mengambil satu job siap jalan: queued, retry_wait yang sudah waktunya, atau running dengan
// lease kedaluwarsa (worker sebelumnya mati). Job pada database yang sedang dikerjakan job lain
// dilewati. Mengembalikan nil jika tidak ada pekerjaan.
func Claim(ctx context.Context, db *pgxpool.Pool, workerID string, lease time.Duration) (*Job, error) {
	ctx = telemetry.Quiet(ctx) // polling tiap detik: dicatat di level Debug, bukan Info
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
		UPDATE jobs SET status = 'running', worker_id = $2, lease_until = clock_timestamp() + $3::interval,
			started_at = coalesce(started_at, clock_timestamp()), updated_at = clock_timestamp(),
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

const mine = ` WHERE id = $1 AND worker_id = $2 AND status = 'running'`

// Heartbeat memperpanjang lease. ErrLeaseLost berarti job sudah diambil worker lain.
func Heartbeat(ctx context.Context, q Querier, id int64, worker string, lease time.Duration) error {
	ctx = telemetry.Quiet(ctx)
	return fenced(ctx, q, `UPDATE jobs SET lease_until = now() + $3::interval, updated_at = now()`+mine, id, worker, lease.String())
}

// SetExecution mencatat attempt aktif (execution history milik workflow).
func SetExecution(ctx context.Context, q Querier, id int64, worker, executionID string, attempt int) error {
	return fenced(ctx, q, `UPDATE jobs SET execution_id = $3::uuid, attempt = $4, attempt_open = true, updated_at = now()`+mine, id, worker, executionID, attempt)
}

// CloseAttempt menandai hasil attempt aktif sudah tercatat di history.
func CloseAttempt(ctx context.Context, q Querier, id int64, worker string) error {
	return fenced(ctx, q, `UPDATE jobs SET attempt_open = false, updated_at = now()`+mine, id, worker)
}

// SetProgress menyinkronkan ringkasan progres dari bukti di target (idempoten: nilai absolut).
func SetProgress(ctx context.Context, q Querier, id int64, worker string, batches, rows int64, total *int64, hold string) error {
	return fenced(ctx, q, `UPDATE jobs SET committed_batches = $3, committed_rows = $4, total_rows = $5, hold = $6,
		updated_at = now()`+mine, id, worker, batches, rows, total, hold)
}

func Succeed(ctx context.Context, q Querier, id int64, worker string) error {
	return fenced(ctx, q, `UPDATE jobs SET status = 'success', hold = 'none', finished_at = now(), lease_until = NULL,
		error_message = NULL, attempt_open = false, updated_at = now()`+mine, id, worker)
}

// Fail mengakhiri job. hold menjelaskan keadaan database (partial, commit_unknown, ...).
func Fail(ctx context.Context, q Querier, id int64, worker, hold, msg string) error {
	return fenced(ctx, q, `UPDATE jobs SET status = 'failed', hold = $3, error_message = $4, finished_at = now(),
		lease_until = NULL, attempt_open = false, updated_at = now()`+mine, id, worker, hold, msg)
}

// Retry menjadwalkan ulang setelah jeda. Mengembalikan false jika jatah retry habis
// (job sudah ditandai failed dengan hold yang diberikan).
func Retry(ctx context.Context, q Querier, j *Job, worker, hold, msg string, backoff time.Duration) (bool, error) {
	if j.RetryCount >= j.MaxRetries {
		return false, Fail(ctx, q, j.ID, worker, hold, msg)
	}
	return true, fenced(ctx, q, `UPDATE jobs SET status = 'retry_wait', retry_count = retry_count + 1, hold = $3,
		error_message = $4, next_retry_at = now() + $5::interval, worker_id = NULL, lease_until = NULL,
		attempt_open = false, updated_at = now()`+mine, j.ID, worker, hold, msg, backoff.String())
}
