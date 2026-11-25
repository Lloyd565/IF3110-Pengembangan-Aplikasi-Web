// Package worker mengambil job dari queue dan menjalankannya di Target DB. Urutan per job:
// claim (lease) -> revalidasi izin/approval/revisi -> catat attempt -> lock target -> eksekusi
// -> bukti commit di target -> hasil attempt di history -> status job. Worker memakai context
// job sendiri, bukan context HTTP yang sudah selesai.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/batch"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/jobs"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

// Flow adalah bagian workflow (milik Richard) yang dibutuhkan worker; workflow.Service memenuhinya.
type Flow interface {
	Prepare(ctx context.Context, requestID string) (workflow.Plan, error)
	BeginAttempt(ctx context.Context, pl workflow.Plan, jobID string, attemptNo int) (string, error)
	FinishAttempt(ctx context.Context, executionID, status, errMsg string) error
}

// Recoverer opsional: revalidasi untuk resume. Pengajuan semula tetap harus approved, tetapi
// penahan posisi (partial/commit_unknown) justru yang sedang dipulihkan, jadi tidak ikut diperiksa.
type Recoverer interface {
	PrepareRecovery(ctx context.Context, requestID string) (workflow.Plan, error)
}

type Targets interface {
	Target(ctx context.Context, id uuid.UUID) (*dbconn.Target, error)
}

type Metrics interface {
	batch.Metrics
	QueueWait(time.Duration)
	Retried()
	Finished(outcome string, d time.Duration)
}

type Worker struct {
	DB      *pgxpool.Pool
	Flow    Flow
	Targets Targets
	ID      string

	Lease            time.Duration
	Poll             time.Duration
	LockWait         time.Duration
	StatementTimeout time.Duration
	Backoff          func(retry int) time.Duration

	ReconcileWait time.Duration

	Hooks   batch.Hooks // hanya untuk failure injection pada test
	Metrics Metrics
}

func (w *Worker) lease() time.Duration {
	if w.Lease > 0 {
		return w.Lease
	}
	return 30 * time.Second
}

func (w *Worker) backoff(retry int) time.Duration {
	if w.Backoff != nil {
		return w.Backoff(retry)
	}
	return min(time.Second<<min(retry, 6), time.Minute)
}

// Run memproses job sampai ctx dibatalkan (shutdown).
func (w *Worker) Run(ctx context.Context) error {
	poll := w.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for ctx.Err() == nil {
		did, err := w.RunOnce(ctx)
		if err != nil {
			slog.Error("worker error", "event", "worker.error", "status", "error", "error", err.Error(), "worker", w.ID)
		}
		if err != nil || !did {
			select {
			case <-ctx.Done():
			case <-time.After(poll):
			}
		}
	}
	return ctx.Err()
}

// RunOnce mengklaim dan memproses satu job. false berarti tidak ada pekerjaan.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	j, err := jobs.Claim(ctx, w.DB, w.ID, w.lease())
	if err != nil || j == nil {
		return false, err
	}
	w.process(ctx, j)
	return true, nil
}

type result int

const (
	permanent result = iota
	transient
	unknown
)

func classify(err error) result {
	switch {
	case errors.Is(err, batch.ErrCommitUnknown):
		return unknown
	case errors.Is(err, batch.ErrUnsafe), errors.Is(err, batch.ErrInvalidSpec):
		return permanent
	case batch.IsTransient(err):
		return transient
	case batch.IsDefinite(err):
		return permanent
	}
	return transient // jaringan putus dsb: bukti di target mencegah double commit, aman diulang
}

func (w *Worker) process(parent context.Context, j *jobs.Job) {
	started := time.Now()
	ctx, cancel := context.WithCancel(jobs.ExtractTrace(parent, j.TraceCtx))
	defer cancel()
	ctx, span := telemetry.Tracer().Start(ctx, "job.run")
	defer span.End()

	var snap workflow.Plan
	_ = json.Unmarshal(j.Plan, &snap)
	info := telemetry.Info{Actor: strconv.FormatInt(snap.ActorID, 10), Database: j.ConnectionID.String(), CorrelationID: j.RequestID.String(),
		MigrationID: snap.MigrationID, RequestID: j.RequestID.String(), JobID: j.ID, Attempt: j.Attempt + 1, Op: snap.Action}
	ctx = telemetry.With(ctx, info)
	if w.Metrics != nil && j.RetryCount == 0 {
		w.Metrics.QueueWait(time.Since(j.QueuedAt))
	}
	telemetry.Event(ctx, slog.LevelInfo, "job.claimed", "running", slog.String("kind", j.Kind), slog.Int("retry", j.RetryCount))

	var lost atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	hctx := ctx // salinan: ctx milik proses ditimpa di bawah, goroutine tidak boleh membacanya
	go func() {
		t := time.NewTicker(w.lease() / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-hctx.Done():
				return
			case <-t.C:
				if err := jobs.Heartbeat(hctx, w.DB, j.ID, w.ID, w.lease()); errors.Is(err, jobs.ErrLeaseLost) {
					lost.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	// attempt yang ditinggal worker sebelumnya (mati sebelum mencatat hasil) ditutup lebih dulu.
	if j.AttemptOpen && j.ExecutionID != "" {
		if err := w.Flow.FinishAttempt(ctx, j.ExecutionID, "failed", "Worker sebelumnya berhenti sebelum mencatat hasil attempt."); err != nil {
			w.retry(ctx, j, jobs.HoldNone, err, &lost)
			return
		}
		if err := jobs.CloseAttempt(ctx, w.DB, j.ID, w.ID); err != nil {
			return
		}
	}

	plan, err := w.prepare(ctx, j)
	if err != nil {
		if isRevalidationRejection(err) {
			w.reject(ctx, j, snap, err, &lost)
			return
		}
		w.retry(ctx, j, jobs.HoldNone, err, &lost)
		return
	}
	info.Op, info.MigrationID, info.Actor = plan.Action, plan.MigrationID, strconv.FormatInt(plan.ActorID, 10)

	attemptNo := j.Attempt + 1
	exec, err := w.Flow.BeginAttempt(ctx, plan, strconv.FormatInt(j.ID, 10), attemptNo)
	if err != nil {
		w.retry(ctx, j, jobs.HoldNone, err, &lost)
		return
	}
	if err := jobs.SetExecution(ctx, w.DB, j.ID, w.ID, exec, attemptNo); err != nil {
		return // lease hilang: worker lain yang menutup attempt ini
	}
	info.ExecutionID, info.Attempt = exec, attemptNo
	ctx = telemetry.With(ctx, info)

	prog, runErr := w.execute(ctx, j, plan, &lost, cancel)
	if lost.Load() {
		telemetry.Event(ctx, slog.LevelWarn, "job.lease_lost", "abandoned")
		return
	}
	w.finish(ctx, j, exec, prog, runErr, started, &lost)
}

func (w *Worker) prepare(ctx context.Context, j *jobs.Job) (workflow.Plan, error) {
	// Job yang membawa hold (partial, commit_unknown, partially_reverted) sedang memulihkan keadaan yang
	// ia tinggalkan sendiri; penahan itu tidak boleh menolaknya. Job tanpa hold tetap diperiksa penuh
	// terhadap posisi database, sehingga urutan linear terjaga.
	if j.Kind == jobs.KindResume || j.Hold != jobs.HoldNone {
		if r, ok := w.Flow.(Recoverer); ok {
			return r.PrepareRecovery(ctx, j.RequestID.String())
		}
	}
	return w.Flow.Prepare(ctx, j.RequestID.String())
}

// isRevalidationRejection: approval/revisi/izin tidak lagi sah. Berbeda dari gangguan sementara
// (database platform mati), penolakan ini tidak akan berubah jika diulang.
func isRevalidationRejection(err error) bool {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Status >= 400 && ce.Status < 500
	}
	return errors.As(err, new(migration.ValidationError)) || errors.Is(err, migration.ErrNotFound)
}

func spec(j *jobs.Job, p workflow.Plan) batch.Spec {
	op := batch.OpUp
	if p.Action == "rollback" {
		op = batch.OpDown
	}
	sp := batch.Spec{RootID: j.RootID, Op: op, SQL: p.SQL}
	if p.Mode == "batch" {
		sp.Size, sp.Table, sp.Column = p.BatchSize, p.Scope, p.CursorColumn
	}
	return sp
}

func (w *Worker) execute(ctx context.Context, j *jobs.Job, plan workflow.Plan, lost *atomic.Bool, cancel context.CancelFunc) (batch.Progress, error) {
	prog := batch.Progress{Batches: j.CommittedBatches, Rows: j.CommittedRows, Total: j.TotalRows}
	tgt, err := w.Targets.Target(ctx, j.ConnectionID)
	if err != nil {
		return prog, err
	}
	if tgt.DBMS != dbconn.PostgreSQL || tgt.Postgres == nil {
		return prog, fmt.Errorf("%w: worker M2 hanya mendukung target PostgreSQL", batch.ErrInvalidSpec)
	}
	r := &batch.Runner{Pool: tgt.Postgres, LockWait: w.LockWait, StatementTimeout: w.StatementTimeout, Hooks: w.Hooks, ReconcileWait: w.ReconcileWait}
	if w.Metrics != nil {
		r.Metrics = w.Metrics
	}
	report := func(p batch.Progress) {
		if err := jobs.SetProgress(ctx, w.DB, j.ID, w.ID, p.Batches, p.Rows, p.Total, jobs.HoldNone); errors.Is(err, jobs.ErrLeaseLost) {
			lost.Store(true)
			cancel()
		}
	}
	sp := spec(j, plan)
	var p batch.Progress
	if j.Kind == jobs.KindCompensate {
		p, err = r.Compensate(ctx, sp, report)
	} else {
		p, err = r.Run(ctx, sp, report)
	}
	if p.Batches < prog.Batches {
		p.Batches, p.Rows = prog.Batches, prog.Rows
	}
	return p, err
}

// holdFor menentukan keadaan database setelah attempt tidak selesai: batch yang sudah committed
// meninggalkan partial (atau partially_reverted pada kompensasi).
func holdFor(j *jobs.Job, prog batch.Progress, err error) (hold, attemptStatus string) {
	switch {
	case errors.Is(err, batch.ErrCommitUnknown),
		// commit_unknown baru berakhir setelah bukti di target berhasil dibaca; selama target belum
		// bisa dikunci, keadaan itu tetap berlaku.
		j.Hold == jobs.HoldCommitUnknown && errors.Is(err, batch.ErrBusy):
		return jobs.HoldCommitUnknown, "commit_unknown"
	case prog.Batches > 0 && j.Kind == jobs.KindCompensate:
		return jobs.HoldPartiallyReverted, "partially_reverted"
	case prog.Batches > 0:
		return jobs.HoldPartial, "partial"
	}
	return jobs.HoldNone, "failed"
}

func (w *Worker) finish(ctx context.Context, j *jobs.Job, exec string, prog batch.Progress, runErr error, started time.Time, lost *atomic.Bool) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := jobs.SetProgress(fctx, w.DB, j.ID, w.ID, prog.Batches, prog.Rows, prog.Total, jobs.HoldNone); errors.Is(err, jobs.ErrLeaseLost) {
		return
	}
	if runErr == nil {
		// Jika hasil tidak bisa dicatat, job dibiarkan: lease habis, worker berikutnya menutup attempt ini
		// lalu menemukan bukti commit di target dan menyelesaikannya tanpa mengulang SQL.
		if err := w.Flow.FinishAttempt(fctx, exec, "success", ""); err != nil {
			slog.Error("gagal mencatat attempt", "event", "attempt.record_failed", "status", "error", "error", err.Error())
			return
		}
		if err := jobs.Succeed(fctx, w.DB, j.ID, w.ID); err == nil {
			w.observeFinished("success", started)
			telemetry.Event(ctx, slog.LevelInfo, "job.finished", "success", slog.Int64("committed_rows", prog.Rows), slog.Int64("committed_batches", prog.Batches))
		}
		return
	}
	hold, status := holdFor(j, prog, runErr)
	if err := w.Flow.FinishAttempt(fctx, exec, status, runErr.Error()); err != nil {
		slog.Error("gagal mencatat attempt", "event", "attempt.record_failed", "status", "error", "error", err.Error())
		return
	}
	if ctx.Err() == nil && classify(runErr) == permanent {
		if err := jobs.Fail(fctx, w.DB, j.ID, w.ID, hold, runErr.Error()); err == nil {
			w.observeFinished("failed", started)
			telemetry.Event(ctx, slog.LevelError, "job.finished", "failed", slog.String("hold", hold), slog.String("error", telemetry.RedactError(runErr)))
		}
		return
	}
	w.retry(ctx, j, hold, runErr, lost) // juga saat shutdown (ctx dibatalkan): job dilepas secepatnya
}

// retry menjadwalkan ulang job, atau menandainya failed bila jatah retry habis. Dipanggil hanya
// setelah attempt (jika ada) tercatat hasilnya.
func (w *Worker) retry(ctx context.Context, j *jobs.Job, hold string, cause error, lost *atomic.Bool) {
	if lost.Load() {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	again, err := jobs.Retry(rctx, w.DB, j, w.ID, hold, cause.Error(), w.backoff(j.RetryCount))
	if err != nil {
		return
	}
	if again {
		if w.Metrics != nil {
			w.Metrics.Retried()
		}
		telemetry.Event(ctx, slog.LevelWarn, "job.retry", "retry_wait", slog.Int("retry", j.RetryCount+1), slog.String("error", telemetry.RedactError(cause)))
		return
	}
	telemetry.Event(ctx, slog.LevelError, "job.finished", "failed", slog.String("hold", hold), slog.String("error", telemetry.RedactError(cause)))
}

// reject mencatat penolakan revalidasi sebagai attempt gagal (history mencatat semua attempt)
// memakai snapshot plan saat enqueue, lalu menutup job tanpa menyentuh Target DB.
func (w *Worker) reject(ctx context.Context, j *jobs.Job, snap workflow.Plan, cause error, lost *atomic.Bool) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	msg := "Revalidasi gagal: " + cause.Error()
	attemptNo := j.Attempt + 1
	if exec, err := w.Flow.BeginAttempt(fctx, snap, strconv.FormatInt(j.ID, 10), attemptNo); err == nil {
		if jobs.SetExecution(fctx, w.DB, j.ID, w.ID, exec, attemptNo) == nil {
			w.Flow.FinishAttempt(fctx, exec, "failed", msg)
		}
	}
	if err := jobs.Fail(fctx, w.DB, j.ID, w.ID, jobs.HoldNone, msg); err == nil && !lost.Load() {
		telemetry.Event(ctx, slog.LevelWarn, "job.rejected", "failed", slog.String("error", msg))
	}
}

func (w *Worker) observeFinished(outcome string, started time.Time) {
	if w.Metrics != nil {
		w.Metrics.Finished(outcome, time.Since(started))
	}
}
