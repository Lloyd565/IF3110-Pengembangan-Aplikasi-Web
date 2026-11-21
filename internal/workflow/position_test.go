package workflow

import (
	"strings"
	"testing"
)

const (
	A int64 = iota + 1
	B
	C
)

func ok(a, m int64, action string) attempt {
	return attempt{ID: id(a), MigrationID: m, Action: action, Status: "success"}
}
func id(n int64) string { return "E" + string(rune('0'+n)) }

func TestRollbackOnlyLastActiveAndLinearForward(t *testing.T) {
	h := []attempt{ok(1, A, "apply"), ok(2, B, "apply"), ok(3, C, "apply")}
	p := replay(h)
	if msg := p.check("rollback", B, "E2"); msg == "" {
		t.Fatal("rollback B sebelum C harus ditolak")
	}
	if msg := p.check("rollback", C, "E3"); msg != "" {
		t.Fatalf("rollback C: %s", msg)
	}
	if msg := p.check("rollback", C, "E1"); msg == "" {
		t.Fatal("rollback dengan execution target yang salah harus ditolak")
	}

	// Rollback C lalu B; forward harus B dulu, baru C.
	rC := attempt{ID: "E4", MigrationID: C, Action: "rollback", Status: "success", Reverts: "E3"}
	rB := attempt{ID: "E5", MigrationID: B, Action: "rollback", Status: "success", Reverts: "E2"}
	p = replay(append(h, rC, rB))
	if msg := p.check("forward", C, "E4"); msg == "" {
		t.Fatal("forward C sebelum B harus ditolak")
	}
	if msg := p.check("forward", B, "E5"); msg != "" {
		t.Fatalf("forward B: %s", msg)
	}
	if msg := p.check("apply", B, ""); !strings.Contains(msg, "forward") {
		t.Fatalf("apply ulang migration yang di-rollback harus diarahkan ke forward, dapat %q", msg)
	}

	// Forward B tidak otomatis menjalankan C; C tetap menunggu forward-nya sendiri.
	p = replay(append(h, rC, rB, attempt{ID: "E6", MigrationID: B, Action: "forward", Status: "success"}))
	if top(p.active) != B || p.check("forward", C, "E4") != "" {
		t.Fatalf("setelah forward B: active=%v undone=%v", p.active, p.undone)
	}
}

func TestFailedAttemptsDoNotMovePosition(t *testing.T) {
	h := []attempt{
		ok(1, A, "apply"),
		{ID: "E2", MigrationID: A, Action: "rollback", Status: "failed", Reverts: "E1"},
		{ID: "E3", MigrationID: B, Action: "apply", Status: "failed"},
	}
	p := replay(h)
	if top(p.active) != A {
		t.Fatalf("rollback gagal tidak boleh menggeser posisi: %v", p.active)
	}
	if p.check("rollback", A, "E1") != "" {
		t.Fatal("rollback A harus tetap bisa diajukan ulang")
	}
	if p.check("apply", B, "") != "" {
		t.Fatal("apply yang gagal boleh dicoba ulang")
	}
}

func TestUnresolvedBlocksUntilReconciled(t *testing.T) {
	h := []attempt{ok(1, A, "apply"), {ID: "E2", MigrationID: B, Action: "apply", Status: "partial"}}
	if replay(h).check("apply", C, "") == "" {
		t.Fatal("partial harus menahan migration lain")
	}
	// Attempt berikutnya untuk B yang sukses (resume) melepas penahanan.
	h = append(h, ok(3, B, "apply"))
	if msg := replay(h).check("apply", C, ""); msg != "" {
		t.Fatalf("setelah rekonsiliasi: %s", msg)
	}
}

func TestHideRollbackBySpecExample(t *testing.T) {
	// Riwayat lengkap: A[E1], B[E2], B rollback E2 [E3], B forward [E4], C[E5], ditambah attempt gagal.
	h := []attempt{
		ok(1, A, "apply"),
		ok(2, B, "apply"),
		{ID: "E3", MigrationID: B, Action: "rollback", Status: "success", Reverts: "E2"},
		ok(4, B, "forward"),
		{ID: "E6", MigrationID: C, Action: "apply", Status: "failed"},
		ok(5, C, "apply"),
	}
	got := hideRollback(h, func(a attempt) attempt { return a })
	var ids []string
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "E1,E4,E6,E5" {
		t.Fatalf("hide rollback = %v, mau E1,E4,E6,E5", ids)
	}
	if len(h) != 6 {
		t.Fatal("filter tidak boleh mengubah data asli")
	}
}
