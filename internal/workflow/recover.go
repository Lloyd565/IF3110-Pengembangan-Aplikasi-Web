package workflow

import (
	"context"
	"errors"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

func (s *Service) Revalidate(ctx context.Context, requestID string, recovery bool, resumeActorID int64) error {
	if recovery {
		_, err := s.PrepareRecovery(ctx, requestID, resumeActorID)
		return err
	}
	_, err := s.Prepare(ctx, requestID)
	return err
}

func (s *Service) PrepareRecovery(ctx context.Context, requestID string, resumeActorID int64) (Plan, error) {
	id, err := parseUUID(requestID)
	if err != nil {
		return Plan{}, err
	}
	x, err := scanRequest(s.DB.QueryRow(ctx, selectRequest+` WHERE q.id = $1`, id))
	if err != nil {
		return Plan{}, err
	}
	if x.stored != "queued" || x.queuedBy == nil {
		return Plan{}, conflict("Pengajuan belum masuk queue.")
	}
	x.stored = "pending_review"
	if err := evaluate(ctx, s.DB, &x); err != nil {
		return Plan{}, err
	}
	if x.Status != "approved" {
		return Plan{}, conflict("Approval tidak lagi sah (revisi, leader, atau grant berubah).")
	}
	if err := s.validateTarget(ctx, s.DB, x.connID, &x, false); err != nil {
		return Plan{}, err
	}
	if resumeActorID == 0 {
		resumeActorID = *x.queuedBy
	}
	actor := reqctx.Principal{UserID: resumeActorID}
	if err := authz.CanUsing(ctx, s.DB, actor, x.connID, authz.MigrationExecute); err != nil {
		if errors.Is(err, authz.ErrForbidden) {
			return Plan{}, forbidden("Pengguna yang mengirim ke queue tidak lagi memiliki izin eksekusi.")
		}
		return Plan{}, err
	}
	return plan(x, actor.UserID), nil
}
