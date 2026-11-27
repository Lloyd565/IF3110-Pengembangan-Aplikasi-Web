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

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/batch"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/jobs"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

type Flow interface {
	Prepare(ctx context.Context, requestID string) (workflow.Plan, error)
	BeginAttempt(ctx context.Context, pl workflow.Plan, jobID string, attemptNo int) (string, error)
	FinishAttempt(ctx context.Context, executionID, status, errMsg string) error
}

type Recoverer interface {
	PrepareRecovery(ctx context.Context, requestID string, resumeActorID int64) (workflow.Plan, error)
}

type Revalidator interface {
	Revalidate(ctx context.Context, requestID string, recovery bool, resumeActorID int64) error
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

	Hooks   batch.Hooks
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

func (w *Worker) Run(ctx context.Context) error {
	poll := w.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for ctx.Err() == nil {
		did, err := w.RunOnce(ctx)
		if err != nil {
			slog.Error("worker error", "event", "worker.error", "status", "error", "error", telemetry.RedactError(err), "worker", w.ID)
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

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	owner := *w
	owner.ID = w.ID + ":" + uuid.NewString()
	j, err := jobs.Claim(ctx, w.DB, owner.ID, w.lease())
	if err != nil || j == nil {
		return false, err
	}
	owner.process(ctx, j)
	return true, nil
}

var errUnsupportedTarget = errors.New("Target worker harus PostgreSQL atau MariaDB yang terhubung.")

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
	case errors.Is(err, errUnsupportedTarget), errors.Is(err, batch.ErrUnsafe), errors.Is(err, batch.ErrInvalidSpec), errors.Is(err, dbconn.ErrTargetIdentity), errors.Is(err, dbconn.ErrUnknownConnection):
		return permanent
	case isRevalidationRejection(err):
		return permanent
	case batch.IsTransient(err):
		return transient
	case errors.As(err, new(*mysql.MySQLError)):
		return permanent
	case batch.IsDefinite(err):
		return permanent
	}
	return transient
}

func (w *Worker) process(parent context.Context, j *jobs.Job) {
	started := time.Now()
	ctx, cancel := context.WithCancel(jobs.ExtractTrace(parent, j.TraceCtx))
	defer cancel()
	ctx, span := telemetry.Tracer().Start(ctx, "job.run")
	defer span.End()

	var snap workflow.Plan
	if err := json.Unmarshal(j.Plan, &snap); err != nil {
		hold := j.Hold
		if j.AttemptOpen || j.CommittedBatches > 0 {
			hold = jobs.HoldCommitUnknown
		}
		returnErr := jobs.Fail(ctx, w.DB, j.ID, w.ID, hold, "Snapshot job tidak dapat dibaca.")
		if returnErr != nil {
			telemetry.Event(ctx, slog.LevelWarn, "job.finalize_failed", "awaiting_recovery", slog.String("error", telemetry.RedactError(returnErr)))
		}
		return
	}
	info := telemetry.Info{Actor: strconv.FormatInt(snap.ActorID, 10), Database: j.ConnectionID.String(), CorrelationID: j.RequestID.String(),
		MigrationID: snap.MigrationID, RevisionID: snap.RevisionID, RequestID: j.RequestID.String(), JobID: j.ID, Attempt: j.Attempt + 1, Op: snap.Action}
	ctx = telemetry.With(ctx, info)
	if w.Metrics != nil && j.RetryCount == 0 {
		w.Metrics.QueueWait(time.Since(j.QueuedAt))
	}
	telemetry.Event(ctx, slog.LevelInfo, "job.claimed", "running", slog.String("kind", j.Kind), slog.Int("retry", j.RetryCount))

	var lost atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	hctx := ctx
	go func() {
		t := time.NewTicker(max(w.lease()/3, time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-hctx.Done():
				return
			case <-t.C:
				heartbeatCtx, heartbeatCancel := context.WithTimeout(hctx, max(w.lease()/3, time.Millisecond))
				err := jobs.Heartbeat(heartbeatCtx, w.DB, j.ID, w.ID, w.lease())
				heartbeatCancel()
				if err != nil {
					lost.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	if j.AttemptOpen && j.ExecutionID != "" {
		recovered := info
		recovered.ExecutionID, recovered.Attempt = j.ExecutionID, j.Attempt
		ctx = telemetry.With(ctx, recovered)
		if r, ok := w.Flow.(interface {
			AttemptStatus(context.Context, string, workflow.Plan) (string, error)
		}); ok {
			status, err := r.AttemptStatus(ctx, j.ExecutionID, snap)
			if err != nil {
				w.recoveryFailure(ctx, j, status, err, &lost)
				return
			}
			tgt, err := w.verifiedTarget(ctx, j, snap, true)
			if err != nil {
				w.recoveryFailure(ctx, j, status, err, &lost)
				return
			}
			var progress batch.Progress
			var done bool
			if !j.DispatchReady && status == "running" {
				progress = batch.Progress{Batches: j.CommittedBatches, Rows: j.CommittedRows, Total: j.TotalRows}
			} else if tgt.DBMS == dbconn.PostgreSQL {
				progress, done, err = (&batch.Runner{Pool: tgt.Postgres, LockWait: w.LockWait, EvidenceKey: tgt.EvidenceKey, TargetServer: snap.TargetServer}).Evidence(ctx, spec(j, snap))
			} else {
				progress, done, err = migration.WorkflowEvidence(ctx, tgt, j.RootID, spec(j, snap).Op, spec(j, snap).Binding, snap.TargetServer)
			}
			if err == nil && ((!done && status == "success") || ((done || progress.Batches > 0) && status == "failed") || progress.Batches < j.CommittedBatches) {
				err = batch.ErrCommitUnknown
			}
			if err != nil {
				w.recoveryFailure(ctx, j, status, err, &lost)
				return
			}
			if done {
				w.finish(ctx, j, j.ExecutionID, progress, nil, started, &lost)
				return
			}
			verified := *j
			verified.Hold = jobs.HoldNone
			hold, attemptStatus := holdFor(&verified, progress, nil)
			if err := w.completeAttempt(ctx, j, j.ExecutionID, attemptStatus, "Worker sebelumnya terputus; hasil diperiksa dari bukti target.", func(q jobs.Querier) error {
				if err := jobs.SetProgress(ctx, q, j.ID, w.ID, progress.Batches, progress.Rows, progress.Total, hold); err != nil {
					return err
				}
				return jobs.CloseAttempt(ctx, q, j.ID, w.ID)
			}); err != nil {
				return
			}
			j.Hold, j.CommittedBatches, j.CommittedRows, j.TotalRows = hold, progress.Batches, progress.Rows, progress.Total
		} else {
			if err := w.Flow.FinishAttempt(ctx, j.ExecutionID, "failed", "Worker sebelumnya terputus."); err != nil {
				return
			}
			if err := jobs.CloseAttempt(ctx, w.DB, j.ID, w.ID); err != nil {
				return
			}
		}
	}

	ctx = telemetry.With(ctx, info)
	if j.RetryCount > j.MaxRetries {
		_ = jobs.Fail(ctx, w.DB, j.ID, w.ID, j.Hold, "Batas percobaan worker tercapai.")
		return
	}
	var resumeActorID int64
	if j.Kind == jobs.KindResume {
		resumeActorID = snap.ActorID
	}
	plan, err := w.prepare(ctx, j, resumeActorID)
	plan.EvidenceNonce = snap.EvidenceNonce
	if err == nil && snap.TargetIdentity != "" && plan != snap {
		err = &contracts.Error{Status: 409, Message: "Rencana eksekusi tidak sama dengan pengajuan pada queue."}
	}
	if err != nil {
		if isRevalidationRejection(err) {
			w.reject(ctx, j, snap, err, &lost)
			return
		}
		w.retry(ctx, j, j.Hold, err, &lost)
		return
	}
	info.Op, info.MigrationID, info.Actor = plan.Action, plan.MigrationID, strconv.FormatInt(plan.ActorID, 10)
	info.RevisionID = plan.RevisionID

	attemptNo := j.Attempt + 1
	exec, err := w.beginAttempt(ctx, j, plan, attemptNo)
	if err != nil {
		w.retry(ctx, j, j.Hold, err, &lost)
		return
	}

	info.ExecutionID, info.Attempt = exec, attemptNo
	ctx = telemetry.With(ctx, info)

	prog, runErr := w.execute(ctx, j, plan, resumeActorID, &lost, cancel)
	if lost.Load() {
		telemetry.Event(ctx, slog.LevelWarn, "job.lease_lost", "abandoned")
		return
	}
	w.finish(ctx, j, exec, prog, runErr, started, &lost)
}

func (w *Worker) prepare(ctx context.Context, j *jobs.Job, resumeActorID int64) (workflow.Plan, error) {

	if j.Kind == jobs.KindResume || j.Hold != jobs.HoldNone {
		if r, ok := w.Flow.(Recoverer); ok {
			return r.PrepareRecovery(ctx, j.RequestID.String(), resumeActorID)
		}
	}
	return w.Flow.Prepare(ctx, j.RequestID.String())
}

func isRevalidationRejection(err error) bool {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Status >= 400 && ce.Status < 500
	}
	return errors.As(err, new(migration.ValidationError)) || errors.Is(err, migration.ErrNotFound) || errors.Is(err, authz.ErrForbidden)
}

func spec(j *jobs.Job, p workflow.Plan) batch.Spec {
	op := batch.OpUp
	if p.Action == "rollback" {
		op = batch.OpDown
	}
	sp := batch.Spec{RootID: j.RootID, Op: op, SQL: p.SQL, Binding: p.TargetIdentity + ":" + p.TargetServer + ":" + p.EvidenceNonce}
	if p.Mode == "batch" {
		sp.Size, sp.Table, sp.Column = p.BatchSize, p.Scope, p.CursorColumn
	}
	return sp
}

func (w *Worker) execute(ctx context.Context, j *jobs.Job, plan workflow.Plan, resumeActorID int64, lost *atomic.Bool, cancel context.CancelFunc) (batch.Progress, error) {
	prog := batch.Progress{Batches: j.CommittedBatches, Rows: j.CommittedRows, Total: j.TotalRows}
	if err := w.checkJobPosition(ctx, j, plan); err != nil {
		return prog, err
	}
	tgt, err := w.verifiedTarget(ctx, j, plan, false)
	if err != nil {
		return prog, err
	}
	var revalidate func(context.Context) error
	if f, ok := w.Flow.(Revalidator); ok {
		revalidate = func(ctx context.Context) error {
			if err := w.checkJobPosition(ctx, j, plan); err != nil {
				return err
			}
			recovery := j.Kind != jobs.KindRun || j.Hold != jobs.HoldNone
			return f.Revalidate(ctx, j.RequestID.String(), recovery, resumeActorID)
		}
	}
	if tgt.DBMS == dbconn.MariaDB && tgt.Maria != nil {
		if plan.Mode == "batch" {
			return prog, fmt.Errorf("%w: cursor batch memerlukan PostgreSQL", batch.ErrInvalidSpec)
		}
		if j.Hold == jobs.HoldCommitUnknown {
			p, done, err := migration.WorkflowEvidence(ctx, tgt, j.RootID, spec(j, plan).Op, spec(j, plan).Binding, plan.TargetServer)
			if err != nil {
				return prog, batch.ErrCommitUnknown
			}
			if done {
				return p, nil
			}
			if j.Kind != jobs.KindResume {
				return p, batch.ErrCommitUnknown
			}
		}
		return migration.RunWorkflowMaria(ctx, tgt, j.RootID, spec(j, plan).Op, plan.SQL, revalidate, func(ctx context.Context) error { return jobs.MarkDispatched(ctx, w.DB, j.ID, w.ID) }, spec(j, plan).Binding, plan.TargetServer)
	}
	if tgt.DBMS != dbconn.PostgreSQL || tgt.Postgres == nil {
		return prog, errUnsupportedTarget
	}
	r := &batch.Runner{Pool: tgt.Postgres, LockWait: w.LockWait, StatementTimeout: w.StatementTimeout, Hooks: w.Hooks, ReconcileWait: w.ReconcileWait, EvidenceKey: tgt.EvidenceKey, TargetServer: plan.TargetServer,
		Revalidate: revalidate,
		Arm:        func(ctx context.Context) error { return jobs.MarkDispatched(ctx, w.DB, j.ID, w.ID) }}
	if _, ok := w.Flow.(atomicFlow); ok {
		r.RecordSQL = func(ctx context.Context, sql string) error {
			return jobs.RecordExecutionSQL(ctx, w.DB, j.ID, w.ID, telemetry.From(ctx).ExecutionID, sql)
		}
	}
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
	if j.Hold == jobs.HoldCommitUnknown || (len(tgt.EvidenceKey) > 0 && j.CommittedBatches > 0) {
		evidence, done, proofErr := r.Evidence(ctx, sp)
		if proofErr != nil || evidence.Batches < prog.Batches {
			return prog, batch.ErrCommitUnknown
		}
		if done {
			return evidence, nil
		}
		if j.Hold == jobs.HoldCommitUnknown && j.Kind != jobs.KindResume {
			return evidence, batch.ErrCommitUnknown
		}
	}
	if j.Kind == jobs.KindCompensate {
		p, err = r.Compensate(ctx, sp, report)
	} else {
		p, err = r.Run(ctx, sp, report)
	}
	if err != nil {
		ectx, stop := context.WithTimeout(context.WithoutCancel(ctx), max(w.ReconcileWait, 3*time.Second))
		evidence, _, proofErr := r.Evidence(ectx, sp)
		stop()
		if proofErr == nil && evidence.Batches > p.Batches {
			p = evidence
		}
	}
	if p.Batches < prog.Batches {
		p.Batches, p.Rows = prog.Batches, prog.Rows
	}
	return p, err
}

func holdFor(j *jobs.Job, prog batch.Progress, err error) (hold, attemptStatus string) {
	switch {
	case errors.Is(err, batch.ErrCommitUnknown),

		j.Hold == jobs.HoldCommitUnknown:
		return jobs.HoldCommitUnknown, "commit_unknown"
	case j.Kind == jobs.KindCompensate && (prog.Batches > 0 || errors.Is(err, batch.ErrUnsafe)):
		return jobs.HoldPartiallyReverted, "partially_reverted"
	case prog.Batches > 0:
		return jobs.HoldPartial, "partial"
	}
	return jobs.HoldNone, "failed"
}

func (w *Worker) finish(ctx context.Context, j *jobs.Job, exec string, prog batch.Progress, runErr error, started time.Time, lost *atomic.Bool) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	status, hold, msg := "success", jobs.HoldNone, ""
	var plan workflow.Plan
	if runErr == nil {
		if json.Unmarshal(j.Plan, &plan) == nil && plan.TargetIdentity != "" {
			if err := w.checkTarget(fctx, j, plan, true); err != nil {
				runErr = batch.ErrCommitUnknown
			}
		}
	}
	if runErr != nil {
		hold, status = holdFor(j, prog, runErr)
		msg = executionMessage(runErr)
	}
	retried := false
	err := w.completeAttempt(fctx, j, exec, status, msg, func(q jobs.Querier) error {
		if err := jobs.SetProgress(fctx, q, j.ID, w.ID, prog.Batches, prog.Rows, prog.Total, hold); err != nil {
			return err
		}
		if runErr == nil {
			if plan.TargetIdentity != "" {
				if err := w.checkTargetUsing(fctx, q, j, plan, true, true); err != nil {
					return err
				}
			}
			return jobs.Succeed(fctx, q, j.ID, w.ID)
		}
		if ctx.Err() == nil && classify(runErr) == permanent {
			return jobs.Fail(fctx, q, j.ID, w.ID, hold, msg)
		}
		var err error
		retried, err = jobs.Retry(fctx, q, j, w.ID, hold, msg, w.backoff(j.RetryCount))
		return err
	})
	if err != nil {
		if errors.Is(err, dbconn.ErrUnknownConnection) || errors.Is(err, pgx.ErrNoRows) {
			w.recoveryFailure(fctx, j, "", err, lost)
			return
		}
		if !errors.Is(err, jobs.ErrLeaseLost) && runErr == nil {
			telemetry.Event(ctx, slog.LevelWarn, "job.finalize_failed", "awaiting_recovery", slog.String("error", telemetry.RedactError(err)))
		}
		return
	}
	if retried {
		if w.Metrics != nil {
			w.Metrics.Retried()
		}
		telemetry.Event(ctx, slog.LevelWarn, "job.retry", "retry_wait", slog.Int("retry", j.RetryCount+1))
		return
	}
	outcome := "success"
	if runErr != nil {
		outcome = "failed"
	}
	w.observeFinished(outcome, started)
	telemetry.Event(ctx, slog.LevelInfo, "job.finished", outcome, slog.String("hold", hold))
}

func (w *Worker) retry(ctx context.Context, j *jobs.Job, hold string, cause error, lost *atomic.Bool) {
	if lost.Load() {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	again, err := jobs.Retry(rctx, w.DB, j, w.ID, hold, executionMessage(cause), w.backoff(j.RetryCount))
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

func (w *Worker) reject(ctx context.Context, j *jobs.Job, snap workflow.Plan, cause error, lost *atomic.Bool) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	msg := "Revalidasi gagal: " + executionMessage(cause)
	hold, status := holdFor(j, batch.Progress{Batches: j.CommittedBatches, Rows: j.CommittedRows}, cause)
	if j.Hold != jobs.HoldNone {
		hold, status = j.Hold, j.Hold
	}
	attemptNo := j.Attempt + 1
	exec, err := w.beginAttempt(fctx, j, snap, attemptNo)
	if err != nil {
		return
	}
	if err := w.completeAttempt(fctx, j, exec, status, msg, func(q jobs.Querier) error { return jobs.Fail(fctx, q, j.ID, w.ID, hold, msg) }); err == nil && !lost.Load() {
		info := telemetry.From(ctx)
		info.ExecutionID, info.Attempt = exec, attemptNo
		telemetry.Event(telemetry.With(ctx, info), slog.LevelWarn, "job.rejected", "failed")
	}

}

func executionMessage(err error) string {
	if errors.Is(err, errUnsupportedTarget) {
		return errUnsupportedTarget.Error()
	}
	if errors.Is(err, batch.ErrCommitUnknown) {
		return batch.ErrCommitUnknown.Error()
	}
	if errors.Is(err, batch.ErrBusy) {
		return batch.ErrBusy.Error()
	}
	if errors.Is(err, batch.ErrUnsafe) {
		return batch.ErrUnsafe.Error()
	}
	if errors.Is(err, batch.ErrInvalidSpec) {
		return batch.ErrInvalidSpec.Error()
	}
	var c *contracts.Error
	if errors.As(err, &c) && c.Status >= 400 && c.Status < 500 {
		return "Izin, approval, revisi, atau target tidak lagi sesuai."
	}
	return telemetry.RedactError(err)
}

func (w *Worker) checkJobPosition(ctx context.Context, j *jobs.Job, p workflow.Plan) error {
	if p.TargetIdentity == "" {
		return nil
	}
	var conflicting bool
	err := w.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs a
 WHERE a.root_job_id<>$1 AND (a.connection_id=$2 OR ($3<>'' AND a.plan->>'TargetServer'=$3))
 AND a.status IN ('failed','retry_wait') AND a.hold<>'none'
 AND NOT EXISTS(SELECT 1 FROM jobs b WHERE b.root_job_id=a.root_job_id AND b.id>a.id AND b.status='success'))`, j.RootID, j.ConnectionID, p.TargetServer).Scan(&conflicting)
	if err != nil {
		return err
	}
	if conflicting {
		return &contracts.Error{Status: 409, Message: "Target memiliki execution lain yang belum direkonsiliasi."}
	}
	return nil
}

func (w *Worker) checkTarget(ctx context.Context, j *jobs.Job, p workflow.Plan, evidence bool) error {
	return w.checkTargetUsing(ctx, w.DB, j, p, evidence, false)
}

func (w *Worker) checkTargetUsing(ctx context.Context, q dbconn.Querier, j *jobs.Job, p workflow.Plan, evidence, lock bool) error {
	if p.TargetIdentity == "" {
		if _, real := w.Flow.(*workflow.Service); !real {
			return nil
		}
		return dbconn.ErrUnknownConnection
	}
	var identity, configuration string
	query := `SELECT ` + dbconn.IdentityFingerprint + `,` + dbconn.ConfigurationFingerprint + ` FROM db_connections dc WHERE id=$1 AND deleted_at IS NULL`
	if lock {
		query += " FOR SHARE"
	}
	err := q.QueryRow(ctx, query, j.ConnectionID).Scan(&identity, &configuration)
	if err != nil {
		return err
	}
	if p.ConnectionID != j.ConnectionID || p.RequestID != j.RequestID.String() || p.TargetIdentity != identity || (!evidence && p.TargetFingerprint != configuration) {
		return dbconn.ErrUnknownConnection
	}
	return nil
}

func (w *Worker) verifiedTarget(ctx context.Context, j *jobs.Job, p workflow.Plan, evidence bool) (*dbconn.Target, error) {
	if err := w.checkTarget(ctx, j, p, evidence); err != nil {
		return nil, err
	}
	tgt, err := w.Targets.Target(ctx, j.ConnectionID)
	if err != nil {
		return nil, err
	}
	if tgt == nil || (tgt.Identity != "" && (p.TargetServer == "" || p.EvidenceNonce == "")) || (tgt.Identity != "" && tgt.Identity != p.TargetIdentity) || (!evidence && tgt.Configuration != "" && tgt.Configuration != p.TargetFingerprint) {
		return nil, dbconn.ErrUnknownConnection
	}
	return tgt, nil
}

func (w *Worker) recoveryFailure(ctx context.Context, j *jobs.Job, status string, cause error, lost *atomic.Bool) {
	if lost.Load() {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	msg := "Bukti target tidak dapat diverifikasi; rekonsiliasi diperlukan."
	if j.RetryCount < j.MaxRetries && !errors.Is(cause, pgx.ErrNoRows) && !errors.Is(cause, dbconn.ErrUnknownConnection) && classify(cause) == transient {
		if err := jobs.RetryEvidence(fctx, w.DB, j, w.ID, msg, w.backoff(j.RetryCount)); err == nil {
			return
		}
	}
	finish := func(q jobs.Querier) error { return jobs.Fail(fctx, q, j.ID, w.ID, jobs.HoldCommitUnknown, msg) }
	if status == "success" || status == "failed" {
		_ = finish(w.DB)
		return
	}
	if errors.Is(cause, pgx.ErrNoRows) {
		_ = pgx.BeginFunc(fctx, w.DB, func(tx pgx.Tx) error {
			if _, err := tx.Exec(fctx, `UPDATE migration_executions SET status='commit_unknown',
				error_message=$4, finished_at=clock_timestamp()
				WHERE id=$1::uuid AND job_id=$2 AND connection_id=$3
				AND status IN ('running','partial','partially_reverted')`,
				j.ExecutionID, strconv.FormatInt(j.ID, 10), j.ConnectionID, msg); err != nil {
				return err
			}
			return finish(tx)
		})
		return
	}
	if err := w.completeAttempt(fctx, j, j.ExecutionID, "commit_unknown", msg, finish); errors.Is(err, pgx.ErrNoRows) || isRevalidationRejection(err) {
		_ = finish(w.DB)
	}
}

func (w *Worker) observeFinished(outcome string, started time.Time) {
	if w.Metrics != nil {
		w.Metrics.Finished(outcome, time.Since(started))
	}
}
