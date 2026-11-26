package worker

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/jobs"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

type atomicFlow interface {
	BeginAttemptAtomic(context.Context, workflow.Plan, string, int, func(pgx.Tx, string) error) (string, error)
	FinishAttemptAtomic(context.Context, string, string, string, func(pgx.Tx) error) error
}

func (w *Worker) beginAttempt(ctx context.Context, j *jobs.Job, p workflow.Plan, no int) (string, error) {
	if f, ok := w.Flow.(atomicFlow); ok {
		return f.BeginAttemptAtomic(ctx, p, strconv.FormatInt(j.ID, 10), no, func(tx pgx.Tx, id string) error {
			return jobs.SetExecution(ctx, tx, j.ID, w.ID, id, no)
		})
	}
	id, err := w.Flow.BeginAttempt(ctx, p, strconv.FormatInt(j.ID, 10), no)
	if err == nil {
		err = jobs.SetExecution(ctx, w.DB, j.ID, w.ID, id, no)
	}
	return id, err
}

func (w *Worker) completeAttempt(ctx context.Context, j *jobs.Job, id, status, msg string, finish func(jobs.Querier) error) error {
	if f, ok := w.Flow.(atomicFlow); ok {
		return f.FinishAttemptAtomic(ctx, id, status, msg, func(tx pgx.Tx) error { return finish(tx) })
	}
	if err := w.Flow.FinishAttempt(ctx, id, status, msg); err != nil {
		return err
	}
	return finish(w.DB)
}
