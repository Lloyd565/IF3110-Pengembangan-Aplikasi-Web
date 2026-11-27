package workflow

import "testing"

func TestRecoveryRollbackPassesOnlyTheBlockingExecution(t *testing.T) {
	for _, status := range []string{"partial", "partially_reverted", "commit_unknown"} {
		p := replay([]attempt{ok(1, A, "apply"), {ID: "E2", MigrationID: B, Action: "apply", Status: status}})
		if msg := p.check("rollback", B, "E2"); msg != "" {
			t.Errorf("%s: kompensasi atas execution penahan harus diizinkan: %s", status, msg)
		}
		if msg := p.check("rollback", B, "E1"); msg == "" {
			t.Errorf("%s: rollback dengan target selain execution penahan harus ditolak", status)
		}
		if msg := p.check("apply", C, ""); msg == "" {
			t.Errorf("%s: migration lain tetap harus ditahan", status)
		}
	}
}

func TestCompensationStuckInCommitUnknownCanBeContinued(t *testing.T) {

	p := replay([]attempt{
		ok(1, A, "apply"),
		{ID: "E2", MigrationID: B, Action: "apply", Status: "partial"},
		{ID: "E3", MigrationID: B, Action: "rollback", Status: "commit_unknown", Reverts: "E2"},
	})
	if msg := p.check("rollback", B, "E3"); msg != "" {
		t.Fatalf("kompensasi lanjutan atas E3 harus diizinkan: %s", msg)
	}
	if msg := p.check("rollback", B, "E2"); msg == "" {
		t.Fatal("E2 bukan lagi penahan terakhir; target lama harus ditolak")
	}
	if msg := p.check("apply", C, ""); msg == "" {
		t.Fatal("selama commit_unknown belum direkonsiliasi, migration lain tetap ditahan")
	}
}

func TestFailedAttemptIsNotARecoveryTarget(t *testing.T) {

	p := replay([]attempt{ok(1, A, "apply"), {ID: "E2", MigrationID: B, Action: "apply", Status: "failed"}})
	if msg := p.check("rollback", B, "E2"); msg == "" {
		t.Fatal("rollback atas attempt failed harus ditolak")
	}
}
