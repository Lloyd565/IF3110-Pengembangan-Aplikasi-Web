package jobs

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

// Queue memenuhi workflow.Queue: enqueue berjalan di transaksi platform milik workflow,
// jadi perubahan status pengajuan dan pembuatan job tidak bisa terpisah.
type Queue struct{ MaxRetries int }

var _ workflow.Queue = Queue{}

func (q Queue) Enqueue(ctx context.Context, tx pgx.Tx, plan workflow.Plan, key string) (contracts.Job, error) {
	rid, err := uuid.Parse(plan.RequestID)
	if err != nil {
		return contracts.Job{}, &contracts.Error{Status: 422, Code: "invalid_request", Message: "ID pengajuan tidak valid."}
	}
	// Rollback yang menargetkan execution partial/partially_reverted adalah kompensasi atas job tersebut:
	// ia memakai root yang sama supaya checkpoint di target tidak tertukar.
	kind, parent := KindRun, (*int64)(nil)
	if plan.Action == "rollback" && plan.TargetExecutionID != "" {
		var jid *string
		err := tx.QueryRow(ctx, `SELECT job_id FROM migration_executions WHERE id = $1::uuid AND (status IN ('partial','partially_reverted','commit_unknown') OR (status='success' AND $2='batch'))`,
			plan.TargetExecutionID, plan.Mode).Scan(&jid)
		if err != nil && err != pgx.ErrNoRows {
			return contracts.Job{}, err
		}
		if jid != nil {
			if id, perr := strconv.ParseInt(*jid, 10, 64); perr == nil {
				kind, parent = KindCompensate, &id
			}
		}
	}
	// Satu pengajuan hanya boleh punya satu job utama, berapa pun kunci idempotensi yang dikirim.
	var existing int64
	err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE request_id = $1 AND kind <> 'resume' ORDER BY id LIMIT 1`, rid).Scan(&existing)
	if err != nil && err != pgx.ErrNoRows {
		return contracts.Job{}, err
	}
	if err == pgx.ErrNoRows {
		plan.EvidenceNonce = uuid.NewString()
		if parent != nil {
			source, err := Get(ctx, tx, *parent)
			if err != nil {
				return contracts.Job{}, err
			}
			var original workflow.Plan
			if err := json.Unmarshal(source.Plan, &original); err != nil {
				return contracts.Job{}, err
			}
			if original.TargetIdentity != plan.TargetIdentity || original.EvidenceNonce == "" {
				return contracts.Job{}, &contracts.Error{Status: 409, Code: "unverified_evidence", Message: "Bukti batch asal tidak dapat diverifikasi."}
			}
			plan.EvidenceNonce = original.EvidenceNonce
		}
		raw, err := json.Marshal(plan)
		if err != nil {
			return contracts.Job{}, err
		}
		existing, _, err = Enqueue(ctx, tx, NewJob{ConnectionID: plan.ConnectionID, RequestID: rid, Key: key, Kind: kind,
			ParentJobID: parent, Plan: raw, MaxRetries: q.MaxRetries, TraceCtx: InjectTrace(ctx)})
		if err != nil {
			return contracts.Job{}, err
		}
	}
	j, err := Get(ctx, tx, existing)
	if err != nil {
		return contracts.Job{}, err
	}
	if j.RequestID != rid {
		return contracts.Job{}, &contracts.Error{Status: 409, Code: "idempotency_conflict", Message: "Idempotency key sudah dipakai untuk pengajuan lain."}
	}
	return Public(j), nil
}

// Public mengubah baris job menjadi DTO kontrak untuk API/SPA.
func Public(j *Job) contracts.Job {
	var plan workflow.Plan
	_ = json.Unmarshal(j.Plan, &plan)
	return contracts.Job{
		ID: strconv.FormatInt(j.ID, 10), DatabaseID: j.ConnectionID.String(), RequestID: j.RequestID.String(),
		MigrationID: plan.MigrationID, ExecutionID: j.ExecutionID, Status: j.PublicStatus(),
		CommittedRows: j.CommittedRows, CommittedBatches: j.CommittedBatches, TotalRows: j.TotalRows,
		Attempt: j.Attempt, LastError: j.Error,
		// commit_unknown dipulihkan lewat resume: worker membaca bukti di target lebih dulu, baru melanjutkan.
		CanResume: j.Status == StatusFailed && (j.Hold == HoldPartial || j.Hold == HoldCommitUnknown) && j.Kind != KindCompensate,
		// Kompensasi yang tertahan commit_unknown dilanjutkan dengan kompensasi lagi: setiap batch down
		// diperiksa dari checkpoint di target dulu, jadi batch yang ternyata committed tidak diulang.
		CanCompensate: j.Status == StatusFailed && (j.Hold == HoldPartial || j.Hold == HoldPartiallyReverted ||
			(j.Hold == HoldCommitUnknown && j.Kind == KindCompensate)),
		UpdatedAt: j.UpdatedAt,
	}
}

// InjectTrace menyimpan W3C trace context (traceparent/tracestate) untuk disambung oleh worker.
func InjectTrace(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return ""
	}
	b, _ := json.Marshal(carrier)
	return string(b)
}

// ExtractTrace memulihkan trace context dari job; context kosong jika tidak ada.
func ExtractTrace(ctx context.Context, stored string) context.Context {
	if stored == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{}
	if json.Unmarshal([]byte(stored), &carrier) != nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
