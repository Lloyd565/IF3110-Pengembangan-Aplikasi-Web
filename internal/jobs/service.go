package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

// Service memenuhi contracts.Jobs untuk API progres dan pemulihan batch.
type Service struct {
	DB    *pgxpool.Pool
	Authz *authz.Authorizer
	Flow  contracts.Workflow
}

var _ contracts.Jobs = (*Service)(nil)

func apiErr(status int, code, msg string) error {
	return &contracts.Error{Status: status, Code: code, Message: msg}
}

func (s *Service) List(ctx context.Context, _ reqctx.Principal, connID uuid.UUID) ([]contracts.Job, error) {
	list, err := List(ctx, s.DB, connID, 200)
	if err != nil {
		return nil, err
	}
	out := make([]contracts.Job, 0, len(list))
	latest := map[int64]bool{}
	for _, j := range list {
		pub := Public(j)
		if latest[j.RootID] {
			pub.CanResume, pub.CanCompensate = false, false
		}
		latest[j.RootID] = true
		out = append(out, pub)
	}
	return out, nil
}

func (s *Service) load(ctx context.Context, connID uuid.UUID, id string) (*Job, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n < 1 {
		return nil, apiErr(404, "not_found", "Job tidak ditemukan.")
	}
	j, err := Get(ctx, s.DB, n)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && j.ConnectionID != connID) {
		return nil, apiErr(404, "not_found", "Job tidak ditemukan.")
	}
	return j, err
}

func (s *Service) Get(ctx context.Context, _ reqctx.Principal, connID uuid.UUID, id string) (contracts.Job, error) {
	j, err := s.load(ctx, connID, id)
	if err != nil {
		return contracts.Job{}, err
	}
	pub := Public(j)
	if pub.CanResume || pub.CanCompensate {
		var newer bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE root_job_id = $1 AND id > $2)`, j.RootID, j.ID).Scan(&newer); err != nil {
			return contracts.Job{}, err
		}
		if newer {
			pub.CanResume, pub.CanCompensate = false, false
		}
	}
	return pub, nil
}

// Recover memulihkan job yang tertahan. resume memakai pengajuan semula (approval-nya tetap
// divalidasi ulang oleh worker) dan hanya memproses batch yang belum committed. compensate membuat
// pengajuan rollback baru atas execution yang tertahan: arah berbeda sehingga seluruh leader harus
// menyetujui ulang sebelum job kompensasi masuk queue.
func (s *Service) Recover(ctx context.Context, p reqctx.Principal, connID uuid.UUID, id, action, key string) (contracts.ChangeRequest, error) {
	if err := s.Authz.Can(ctx, p, connID, authz.MigrationExecute); err != nil {
		return contracts.ChangeRequest{}, apiErr(403, "forbidden", "Anda tidak punya izin menjalankan pemulihan pada database ini.")
	}
	j, err := s.load(ctx, connID, id)
	if err != nil {
		return contracts.ChangeRequest{}, err
	}
	var out contracts.ChangeRequest
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var root int64
		if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id = $1 FOR UPDATE`, j.RootID).Scan(&root); err != nil {
			return err
		}
		current, err := Get(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		var newer bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE root_job_id = $1 AND id > $2)`, root, j.ID).Scan(&newer); err != nil {
			return err
		}
		if newer {
			return apiErr(409, "conflict", "Job ini sudah memiliki pemulihan. Gunakan job pemulihan terbaru.")
		}
		out, err = s.recover(ctx, p, current, action, key, tx)
		return err
	})
	return out, err
}

func (s *Service) recover(ctx context.Context, p reqctx.Principal, j *Job, action, key string, tx pgx.Tx) (contracts.ChangeRequest, error) {
	connID, id := j.ConnectionID, strconv.FormatInt(j.ID, 10)
	pub := Public(j)
	var plan workflow.Plan
	if err := json.Unmarshal(j.Plan, &plan); err != nil {
		return contracts.ChangeRequest{}, err
	}
	switch action {
	case "resume":
		if !pub.CanResume {
			return contracts.ChangeRequest{}, apiErr(409, "conflict", "Job ini tidak bisa dilanjutkan pada statusnya sekarang.")
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE parent_job_id = $1 AND kind = 'resume'
			AND status NOT IN ('success','failed','cancelled'))`, j.ID).Scan(&active); err != nil {
			return contracts.ChangeRequest{}, err
		}
		if active {
			return contracts.ChangeRequest{}, apiErr(409, "conflict", "Sudah ada job lanjutan yang sedang berjalan.")
		}
		if _, _, err := Enqueue(ctx, tx, NewJob{ConnectionID: connID, RequestID: j.RequestID, Kind: KindResume, ParentJobID: &j.ID,
			Key: "resume:" + id + ":" + key, Plan: j.Plan, TraceCtx: InjectTrace(ctx)}); err != nil {
			return contracts.ChangeRequest{}, err
		}
		return s.Flow.Request(ctx, p, connID, j.RequestID.String())
	case "compensate":
		if !pub.CanCompensate || j.ExecutionID == "" {
			return contracts.ChangeRequest{}, apiErr(409, "conflict", "Job ini tidak bisa dikompensasi pada statusnya sekarang.")
		}
		// Idempoten: selama pengajuan kompensasi untuk execution yang sama masih terbuka (draf atau menunggu
		// review), permintaan ulang atau klik ganda mengembalikan pengajuan itu, bukan membuat yang baru.
		var open string
		err := tx.QueryRow(ctx, `SELECT id::text FROM migration_requests WHERE connection_id = $1 AND action = 'rollback'
			AND target_execution_id = $2::uuid AND status IN ('draft','pending_review') ORDER BY created_at DESC LIMIT 1`,
			connID, j.ExecutionID).Scan(&open)
		if err == nil {
			return s.Flow.Request(ctx, p, connID, open)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return contracts.ChangeRequest{}, err
		}
		return s.Flow.Propose(ctx, p, connID, contracts.ChangeInput{
			MigrationID: plan.MigrationID, RevisionID: plan.RevisionID, Action: "rollback", TargetExecutionID: j.ExecutionID,
			Mode: "batch", BatchSize: plan.BatchSize, CursorColumn: plan.CursorColumn, Scope: plan.Scope,
		})
	}
	return contracts.ChangeRequest{}, apiErr(404, "not_found", "Aksi tidak ditemukan.")
}
