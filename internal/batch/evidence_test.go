package batch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/sqlpolicy"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func sealedRunner(t *testing.T, name string) (*Runner, func(string, ...any)) {
	t.Helper()
	db := newTarget(t, name)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return &Runner{Pool: db, EvidenceKey: testKey}, exec
}

// Dengan EvidenceKey, run ulang (duplicate delivery) dan kompensasi tetap berjalan karena bukti
// di-seal pada setiap commit dan diverifikasi sebelum dipakai.
func TestSealedEvidenceRunResumeAndCompensate(t *testing.T) {
	r, _ := sealedRunner(t, "ev_ok")
	ctx := context.Background()
	sp := batchSpec(1, OpUp, upSQL)
	sp.Binding = "identity:server:nonce"
	if _, err := r.Run(ctx, sp, nil); err != nil {
		t.Fatal(err)
	}
	if p, err := r.Run(ctx, sp, nil); err != nil || p.Batches != 10 {
		t.Fatalf("run ulang harus membaca bukti ter-seal, bukan mengulang: %+v err=%v", p, err)
	}
	down := batchSpec(1, OpDown, downSQL)
	down.Binding = sp.Binding
	if p, err := r.Compensate(ctx, down, nil); err != nil || p.Batches != 10 || p.Rows != rows {
		t.Fatalf("kompensasi: %+v err=%v", p, err)
	}
	if b := balances(t, r.Pool); b[100] != rows {
		t.Fatalf("semua baris harus kembali: %v", b)
	}
}

func TestResumeWithLegacySeal(t *testing.T) {
	r, exec := sealedRunner(t, "ev_legacy")
	ctx := context.Background()
	sp := batchSpec(1, OpUp, upSQL)
	sp.Binding = "identity:server:nonce"
	r.Hooks.BeforeCommit = func(_ string, n int) error {
		if n == 3 {
			return errors.New("stop before batch 3")
		}
		return nil
	}
	if progress, err := r.Run(ctx, sp, nil); err == nil || progress.Batches != 2 {
		t.Fatalf("expected two committed batches: %+v, %v", progress, err)
	}
	r.Hooks = Hooks{}

	var state string
	if err := r.Pool.QueryRow(ctx, `SELECT jsonb_build_array(
    (SELECT jsonb_build_array(scope_max,total_rows,finished_at IS NOT NULL)
       FROM _platform.migration_runs WHERE root_job=1 AND op='up'),
    (SELECT jsonb_agg(jsonb_build_array(batch_no,cursor_from,cursor_to,rows_affected,row_manifest) ORDER BY batch_no)
       FROM _platform.migration_checkpoints WHERE root_job=1 AND op='up'))::text`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, testKey)
	mac.Write([]byte(strings.Join([]string{"pg-v1", sp.Binding, "1", OpUp, state}, "\x00")))
	exec(`UPDATE _platform.migration_seals SET proof=$1 WHERE root_job=1 AND op='up'`, mac.Sum(nil))

	if progress, done, err := r.Evidence(ctx, sp); err != nil || done || progress.Batches != 2 {
		t.Fatalf("expected legacy evidence to verify: %+v, %v, %v", progress, done, err)
	}
	if progress, err := r.Run(ctx, sp, nil); err != nil || progress.Batches != 10 || progress.Rows != rows {
		t.Fatalf("expected resume from legacy evidence: %+v, %v", progress, err)
	}
	var proof []byte
	if err := r.Pool.QueryRow(ctx, `SELECT proof FROM _platform.migration_seals WHERE root_job=1 AND op='up'`).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	if len(proof) != sha256.Size+1 || proof[0] != proofV2 || balances(t, r.Pool)[101] != rows {
		t.Fatal("resume must write new evidence and update each row once")
	}
}

// Checkpoint yang diubah di luar platform (atau oleh SQL migration) tidak cocok dengan HMAC, jadi
// worker berhenti di commit_unknown alih-alih mempercayai bukti palsu.
func TestTamperedCheckpointIsNotTrusted(t *testing.T) {
	r, exec := sealedRunner(t, "ev_tamper")
	ctx := context.Background()
	sp := batchSpec(2, OpUp, upSQL)
	r.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 3 {
			return errors.New("berhenti di batch 3")
		}
		return nil
	}
	if _, err := r.Run(ctx, sp, nil); err == nil {
		t.Fatal("prasyarat: run harus berhenti di tengah")
	}
	r.Hooks = Hooks{}
	exec(`UPDATE _platform.migration_checkpoints SET cursor_to = 1000 WHERE root_job = 2 AND batch_no = 2`)
	if _, _, err := r.Evidence(ctx, sp); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("bukti yang diubah harus ditolak: %v", err)
	}
	if _, err := r.Run(ctx, sp, nil); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("resume di atas bukti palsu harus ditolak: %v", err)
	}
	if _, err := r.Compensate(ctx, batchSpec(2, OpDown, downSQL), nil); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("kompensasi di atas bukti palsu harus ditolak: %v", err)
	}
	if b := balances(t, r.Pool); b[101] != 200 || b[100] != 800 {
		t.Fatalf("data tidak boleh berubah setelah bukti dipalsukan: %v", b)
	}
}

// Bukti yang di-seal dengan kunci lain (instance platform lain, atau kunci yang sudah diganti) tidak dipercaya.
func TestEvidenceSealedWithAnotherKeyIsRejected(t *testing.T) {
	r, _ := sealedRunner(t, "ev_key")
	ctx := context.Background()
	sp := batchSpec(3, OpUp, upSQL)
	if _, err := r.Run(ctx, sp, nil); err != nil {
		t.Fatal(err)
	}
	other := &Runner{Pool: r.Pool, EvidenceKey: []byte("kunci-lain-kunci-lain-kunci-lain")}
	if _, _, err := other.Evidence(ctx, sp); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("seal dengan kunci lain harus ditolak: %v", err)
	}
	sp.Binding = "target-lain"
	if _, _, err := r.Evidence(ctx, sp); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("bukti untuk binding target lain harus ditolak: %v", err)
	}
}

// SQL migration tidak bisa menyebut _platform secara langsung (ditolak policy). Lewat SQL dinamis
// namanya bisa disamarkan, tetapi perubahan metadata di dalam transaksi migration terdeteksi dan
// seluruh transaksi dibatalkan, termasuk perubahan datanya.
func TestMigrationSQLCannotTamperWithControlTables(t *testing.T) {
	if err := sqlpolicy.Check(`DELETE FROM _platform.migration_seals`); err == nil {
		t.Fatal("akses langsung ke _platform harus ditolak policy")
	}
	r, exec := sealedRunner(t, "ev_sql")
	ctx := context.Background()
	if _, err := r.Run(ctx, batchSpec(4, OpUp, upSQL), nil); err != nil {
		t.Fatal(err)
	}
	tamper := `DO $$ BEGIN EXECUTE 'DELETE FROM ' || quote_ident('_plat' || 'form') || '.migration_checkpoints'; END $$;
		UPDATE perf.accounts SET balance = 0`
	if err := sqlpolicy.Check(tamper); err != nil {
		t.Fatalf("prasyarat: SQL dinamis lolos policy token (%v); test ini memastikan lapisan berikutnya menangkapnya", err)
	}
	if _, err := r.Run(ctx, Spec{RootID: 5, Op: OpUp, SQL: tamper}, nil); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("perubahan metadata dari SQL migration harus ditolak: %v", err)
	}
	if n := checkpoints(t, r.Pool, 4, OpUp); n != 10 {
		t.Fatalf("checkpoint tidak boleh terhapus: %d", n)
	}
	if b := balances(t, r.Pool); b[101] != rows {
		t.Fatalf("data migration yang menyerang metadata harus ikut dibatalkan: %v", b)
	}

	// Mode batch: fungsi yang dibuat sebelumnya lalu dipanggil dari UPDATE batch juga tertangkap.
	exec(`CREATE FUNCTION perf.sneaky(v bigint) RETURNS bigint LANGUAGE plpgsql AS $$
		BEGIN EXECUTE 'DELETE FROM ' || quote_ident('_plat' || 'form') || '.migration_seals'; RETURN v; END $$`)
	sneaky := `UPDATE perf.accounts SET balance = perf.sneaky(balance) WHERE id > $1 AND id <= $2`
	if _, err := r.Run(ctx, batchSpec(6, OpUp, sneaky), nil); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("batch yang mengubah metadata lewat fungsi harus ditolak: %v", err)
	}
	if _, _, err := r.Evidence(ctx, batchSpec(4, OpUp, upSQL)); err != nil {
		t.Fatalf("bukti migration sebelumnya harus tetap utuh dan autentik: %v", err)
	}
}

// Manifest mencatat versi baris (xmin/ctid). Baris yang diubah pihak lain setelah batch up tidak
// boleh ditimpa oleh kompensasi; kompensasi berhenti tanpa mengubah apa pun.
func TestCompensationRefusesRowsChangedAfterApply(t *testing.T) {
	r, exec := sealedRunner(t, "ev_manifest")
	ctx := context.Background()
	if _, err := r.Run(ctx, batchSpec(7, OpUp, upSQL), nil); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE perf.accounts SET balance = 555 WHERE id = 950`)
	_, err := r.Compensate(ctx, batchSpec(7, OpDown, downSQL), nil)
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("baris yang berubah setelah apply harus menghentikan kompensasi: %v", err)
	}
	if b := balances(t, r.Pool); b[555] != 1 || b[101] != rows-1 {
		t.Fatalf("batch yang tidak aman tidak boleh diubah: %v", b)
	}
	if n := checkpoints(t, r.Pool, 7, OpDown); n != 0 {
		t.Fatalf("batch terakhir (berisi baris 950) dikompensasi lebih dulu dan gagal; tidak ada batch down yang boleh committed: %d", n)
	}
}

func TestBatchReplayTransformation(t *testing.T) {
	up, tracked, err := sqlpolicy.BatchReplay(upSQL, "id", false)
	if err != nil || !tracked || !strings.Contains(up, "RETURNING") || !strings.Contains(up, "xmin") || !strings.Contains(up, "ctid") {
		t.Fatalf("UPDATE forward harus mengembalikan id, xmin, ctid: %q tracked=%v err=%v", up, tracked, err)
	}
	down, tracked, err := sqlpolicy.BatchReplay(`DELETE FROM perf.accounts a WHERE a.id > $1 AND a.id <= $2`, "id", true)
	if err != nil || !tracked || !strings.Contains(down, "ANY($3") || !strings.Contains(down, "a.id") {
		t.Fatalf("DELETE down harus dibatasi ke id manifest memakai alias: %q tracked=%v err=%v", down, tracked, err)
	}
	for _, sql := range []string{
		`DELETE FROM perf.accounts WHERE id > $1 AND id <= $2`,
		`INSERT INTO perf.archive SELECT * FROM perf.accounts WHERE id > $1 AND id <= $2`,
	} {
		if out, tracked, err := sqlpolicy.BatchReplay(sql, "id", false); err != nil || tracked || out != sql {
			t.Errorf("forward non-UPDATE tidak punya manifest: %q tracked=%v err=%v", out, tracked, err)
		}
	}
}
