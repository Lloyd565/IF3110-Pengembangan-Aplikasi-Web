package workflow

import (
	"context"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

func (s *Service) Revalidate(ctx context.Context, requestID string, recovery bool) error {
	if recovery {
		_, err := s.PrepareRecovery(ctx, requestID)
		return err
	}
	_, err := s.Prepare(ctx, requestID)
	return err
}

// PrepareRecovery merevalidasi pengajuan untuk resume job yang tertahan (partial atau
// commit_unknown). Berbeda dari Prepare, penahan posisi tidak ikut diperiksa karena justru
// itulah yang sedang dipulihkan; approval seluruh leader, revisi, dan izin eksekusi tetap wajib sah.
func (s *Service) PrepareRecovery(ctx context.Context, requestID string) (Plan, error) {
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
	actor := reqctx.Principal{UserID: *x.queuedBy}
	if !s.canEdit(ctx, actor, x.connID) {
		return Plan{}, forbidden("Pengguna yang mengirim ke queue tidak lagi memiliki izin eksekusi.")
	}
	return plan(x, actor.UserID), nil
}
