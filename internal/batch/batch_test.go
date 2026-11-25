package batch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

const (
	upSQL   = `UPDATE perf.accounts SET balance = balance + 1 WHERE id > $1 AND id <= $2`
	downSQL = `UPDATE perf.accounts SET balance = balance - 1 WHERE id > $1 AND id <= $2`
	rows    = 1000
)

func newTarget(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	db := testdb.Fresh(t, name)
	if _, err := db.Exec(context.Background(), fmt.Sprintf(`
		CREATE SCHEMA perf;
		CREATE TABLE perf.accounts (id bigint PRIMARY KEY, balance bigint NOT NULL);
		INSERT INTO perf.accounts SELECT g, 100 FROM generate_series(1, %d) g`, rows)); err != nil {
		t.Fatal(err)
	}
	return db
}

func batchSpec(root int64, op, sql string) Spec {
	return Spec{RootID: root, Op: op, SQL: sql, Size: 100, Table: "perf.accounts", Column: "id"}
}

// balances mengembalikan jumlah row per nilai balance, untuk memeriksa tidak ada row yang berubah dua kali.
func balances(t *testing.T, db *pgxpool.Pool) map[int64]int {
	t.Helper()
	out := map[int64]int{}
	rs, err := db.Query(context.Background(), `SELECT balance, count(*) FROM perf.accounts GROUP BY balance`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	for rs.Next() {
		var b int64
		var n int
		rs.Scan(&b, &n)
		out[b] = n
	}
	return out
}

func checkpoints(t *testing.T, db *pgxpool.Pool, root int64, op string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM _platform.migration_checkpoints WHERE root_job = $1 AND op = $2`, root, op).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNormalModeAndDuplicateDelivery(t *testing.T) {
	db := newTarget(t, "batch_normal")
	r := &Runner{Pool: db}
	ctx := context.Background()
	sp := Spec{RootID: 1, Op: OpUp, SQL: `UPDATE perf.accounts SET balance = balance + 1`}
	p, err := r.Run(ctx, sp, nil)
	if err != nil || p.Rows != rows || p.Batches != 1 {
		t.Fatalf("run: %+v err=%v", p, err)
	}
	// pesan terkirim dua kali: tidak boleh double commit.
	p, err = r.Run(ctx, sp, nil)
	if err != nil || p.Rows != rows {
		t.Fatalf("run ulang: %+v err=%v", p, err)
	}
	if b := balances(t, db); len(b) != 1 || b[101] != rows {
		t.Fatalf("setiap row harus berubah tepat sekali: %v", b)
	}
	if _, done, err := r.Evidence(ctx, sp); err != nil || !done {
		t.Errorf("bukti harus menunjukkan selesai: done=%v err=%v", done, err)
	}
}

func TestNormalModeFailureRollsBackEverything(t *testing.T) {
	db := newTarget(t, "batch_normal_fail")
	r := &Runner{Pool: db}
	_, err := r.Run(context.Background(), Spec{RootID: 1, Op: OpUp, SQL: `UPDATE perf.accounts SET balance = balance + 1; SELECT 1/0`}, nil)
	if !IsDefinite(err) {
		t.Fatalf("harus error SQLSTATE, dapat %v", err)
	}
	if b := balances(t, db); len(b) != 1 || b[100] != rows {
		t.Fatalf("kegagalan harus membatalkan seluruh transaksi: %v", b)
	}
	if _, done, _ := r.Evidence(context.Background(), Spec{RootID: 1, Op: OpUp}); done {
		t.Error("tidak boleh ada bukti selesai")
	}
}

func TestBatchCommitsEveryBatchSeparately(t *testing.T) {
	db := newTarget(t, "batch_all")
	r := &Runner{Pool: db}
	var reports []Progress
	p, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), func(p Progress) { reports = append(reports, p) })
	if err != nil || p.Batches != 10 || p.Rows != rows {
		t.Fatalf("progress: %+v err=%v", p, err)
	}
	if p.Total == nil || *p.Total != rows {
		t.Errorf("total harus diketahui: %v", p.Total)
	}
	if len(reports) != 10 || reports[4].Batches != 5 || reports[4].Rows != 500 {
		t.Errorf("progress dilaporkan per batch committed: %+v", reports)
	}
	if b := balances(t, db); len(b) != 1 || b[101] != rows {
		t.Fatalf("balances: %v", b)
	}
	if n := checkpoints(t, db, 1, OpUp); n != 10 {
		t.Errorf("checkpoint = %d, mau 10", n)
	}
	// delivery ganda setelah selesai.
	if p, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); err != nil || p.Batches != 10 {
		t.Fatalf("run ulang: %+v err=%v", p, err)
	}
	if b := balances(t, db); b[101] != rows {
		t.Fatalf("run ulang tidak boleh mengubah data lagi: %v", b)
	}
}

func TestBatchFailureBeforeCommitKeepsPreviousBatchesAndResumes(t *testing.T) {
	db := newTarget(t, "batch_before")
	r := &Runner{Pool: db}
	r.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 4 {
			return errors.New("crash sebelum commit")
		}
		return nil
	}
	p, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil)
	if err == nil || p.Batches != 3 {
		t.Fatalf("harus gagal di batch 4 dengan 3 batch committed: %+v err=%v", p, err)
	}
	if b := balances(t, db); b[101] != 300 || b[100] != 700 {
		t.Fatalf("batch 1-3 tetap committed, sisanya belum: %v", b)
	}
	if n := checkpoints(t, db, 1, OpUp); n != 3 {
		t.Fatalf("checkpoint tidak boleh tertinggal atau mendahului data: %d", n)
	}
	// resume: hanya batch yang belum committed yang diproses.
	r.Hooks.BeforeCommit = nil
	var seen []int
	r.Hooks.AfterCommit = func(op string, n int) error { seen = append(seen, n); return nil }
	p, err = r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil)
	if err != nil || p.Batches != 10 || p.Rows != rows {
		t.Fatalf("resume: %+v err=%v", p, err)
	}
	if len(seen) != 7 || seen[0] != 4 {
		t.Errorf("resume harus mulai dari batch 4 dan hanya 7 batch: %v", seen)
	}
	if b := balances(t, db); len(b) != 1 || b[101] != rows {
		t.Fatalf("tidak boleh ada double commit: %v", b)
	}
}

func TestBatchFailureAfterCommitDoesNotRepeatBatch(t *testing.T) {
	db := newTarget(t, "batch_after")
	r := &Runner{Pool: db}
	r.Hooks.AfterCommit = func(op string, n int) error {
		if n == 4 {
			return errors.New("proses mati setelah commit")
		}
		return nil
	}
	if _, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); err == nil {
		t.Fatal("harus gagal")
	}
	if n := checkpoints(t, db, 1, OpUp); n != 4 {
		t.Fatalf("batch 4 sudah committed bersama checkpoint-nya: %d", n)
	}
	r.Hooks.AfterCommit = nil
	p, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil)
	if err != nil || p.Batches != 10 {
		t.Fatalf("resume: %+v err=%v", p, err)
	}
	if b := balances(t, db); len(b) != 1 || b[101] != rows {
		t.Fatalf("batch 4 tidak boleh diulang: %v", b)
	}
}

func TestBatchScopeFixedAtStart(t *testing.T) {
	db := newTarget(t, "batch_scope")
	r := &Runner{Pool: db}
	r.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 2 {
			return errors.New("berhenti")
		}
		return nil
	}
	r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil)
	// row baru masuk setelah cakupan ditetapkan.
	if _, err := db.Exec(context.Background(), `INSERT INTO perf.accounts SELECT g, 100 FROM generate_series(2001, 2050) g`); err != nil {
		t.Fatal(err)
	}
	r.Hooks.BeforeCommit = nil
	p, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil)
	if err != nil || p.Rows != rows {
		t.Fatalf("hanya row dalam cakupan awal: %+v err=%v", p, err)
	}
	if b := balances(t, db); b[100] != 50 || b[101] != rows {
		t.Fatalf("row baru tidak boleh tersentuh: %v", b)
	}
}

func TestBatchEmptyTable(t *testing.T) {
	db := newTarget(t, "batch_empty")
	db.Exec(context.Background(), `DELETE FROM perf.accounts`)
	p, err := (&Runner{Pool: db}).Run(context.Background(), batchSpec(1, OpUp, upSQL), nil)
	if err != nil || p.Batches != 0 || p.Rows != 0 {
		t.Fatalf("tabel kosong harus langsung selesai: %+v err=%v", p, err)
	}
}

func TestConnectionLostBeforeCommitIsRolledBackThenRetried(t *testing.T) {
	db := newTarget(t, "batch_kill")
	r := &Runner{Pool: db}
	killed := false
	r.Hooks.BeforeCommit = func(op string, n int) error {
		if n != 3 || killed {
			return nil
		}
		killed = true
		_, err := db.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND state = 'idle in transaction'`)
		time.Sleep(200 * time.Millisecond)
		return err
	}
	p, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil)
	if !errors.Is(err, ErrRolledBack) || !IsTransient(err) {
		t.Fatalf("koneksi putus sebelum commit harus ErrRolledBack (aman diulang): %v", err)
	}
	if p.Batches != 2 {
		t.Errorf("batch committed = %d, mau 2", p.Batches)
	}
	r.Hooks.BeforeCommit = nil
	if p, err = r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); err != nil || p.Batches != 10 {
		t.Fatalf("retry: %+v err=%v", p, err)
	}
	if b := balances(t, db); len(b) != 1 || b[101] != rows {
		t.Fatalf("tidak boleh ada double commit: %v", b)
	}
}

func TestReconcileUsesEvidence(t *testing.T) {
	db := newTarget(t, "batch_reconcile")
	r := &Runner{Pool: db}
	if _, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("jawaban commit hilang")
	if err := r.reconcile(context.Background(), batchSpec(1, OpUp, upSQL), 5, cause); err != nil {
		t.Errorf("ada checkpoint: harus dianggap committed, dapat %v", err)
	}
	if err := r.reconcile(context.Background(), batchSpec(1, OpUp, upSQL), 99, cause); !errors.Is(err, ErrRolledBack) {
		t.Errorf("tidak ada checkpoint: harus ErrRolledBack, dapat %v", err)
	}
	// target tidak bisa dibaca: hasil commit belum pasti, jangan menebak.
	dead := newTarget(t, "batch_reconcile_dead")
	dead.Close()
	if err := (&Runner{Pool: dead}).reconcile(context.Background(), batchSpec(1, OpUp, upSQL), 1, cause); !errors.Is(err, ErrCommitUnknown) {
		t.Errorf("target mati: harus ErrCommitUnknown, dapat %v", err)
	}
}

func TestLockSerializesSameDatabaseAcrossRunnersAndAliases(t *testing.T) {
	db := newTarget(t, "batch_lock")
	alias, err := pgxpool.NewWithConfig(context.Background(), db.Config()) // konfigurasi koneksi kedua ke database fisik yang sama
	if err != nil {
		t.Fatal(err)
	}
	defer alias.Close()

	started, release := make(chan struct{}), make(chan struct{})
	first := &Runner{Pool: db}
	first.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 2 {
			close(started)
			<-release // tahan lock di antara commit batch
		}
		return nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := first.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); err != nil {
			t.Error(err)
		}
	}()
	<-started
	for _, pool := range []*pgxpool.Pool{db, alias} {
		_, err := (&Runner{Pool: pool, LockWait: 300 * time.Millisecond}).Run(context.Background(), batchSpec(2, OpUp, upSQL), nil)
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("lock harus melindungi pekerjaan di antara commit batch (alias=%v): %v", pool == alias, err)
		}
	}
	close(release)
	wg.Wait()
	if p, err := (&Runner{Pool: alias}).Run(context.Background(), batchSpec(2, OpUp, upSQL), nil); err != nil || p.Rows != rows {
		t.Fatalf("setelah lock lepas harus bisa jalan: %+v err=%v", p, err)
	}
}

func TestDifferentDatabasesRunInParallel(t *testing.T) {
	a, b := newTarget(t, "batch_par_a"), newTarget(t, "batch_par_b")
	started, release := make(chan struct{}), make(chan struct{})
	ra := &Runner{Pool: a}
	ra.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 1 {
			close(started)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := ra.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); done <- err }()
	<-started
	if p, err := (&Runner{Pool: b, LockWait: 300 * time.Millisecond}).Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); err != nil || p.Rows != rows {
		t.Fatalf("database berbeda tidak boleh saling menunggu: %+v err=%v", p, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCompensateReverseOrderAndResume(t *testing.T) {
	db := newTarget(t, "batch_comp")
	r := &Runner{Pool: db}
	if _, err := r.Run(context.Background(), batchSpec(1, OpUp, upSQL), nil); err != nil {
		t.Fatal(err)
	}
	// gagal di tengah kompensasi: sebagian dikembalikan.
	var order []int
	r.Hooks.BeforeCommit = func(op string, n int) error {
		if op == OpDown {
			order = append(order, n)
			if len(order) == 4 {
				return errors.New("gagal sebagian")
			}
		}
		return nil
	}
	p, err := r.Compensate(context.Background(), Spec{RootID: 1, SQL: downSQL}, nil)
	if err == nil || p.Batches != 3 {
		t.Fatalf("harus berhenti sebagian (3 batch down): %+v err=%v", p, err)
	}
	if want := []int{10, 9, 8, 7}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("urutan batch harus terbalik: %v mau %v", order, want)
	}
	if b := balances(t, db); b[100] != 300 || b[101] != 700 {
		t.Fatalf("batch 8-10 sudah dikembalikan, sisanya belum: %v", b)
	}
	// lanjutkan: partially_reverted bisa diteruskan tanpa mengulang batch yang sudah dikembalikan.
	order = nil
	r.Hooks.BeforeCommit = func(op string, n int) error { order = append(order, n); return nil }
	p, err = r.Compensate(context.Background(), Spec{RootID: 1, SQL: downSQL}, nil)
	if err != nil || p.Batches != 10 {
		t.Fatalf("lanjutan: %+v err=%v", p, err)
	}
	if want := []int{7, 6, 5, 4, 3, 2, 1}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("lanjutan hanya batch yang tersisa: %v mau %v", order, want)
	}
	if b := balances(t, db); len(b) != 1 || b[100] != rows {
		t.Fatalf("seluruh row harus kembali tepat sekali: %v", b)
	}
	// kompensasi ulang tidak mengubah apa pun.
	if _, err := r.Compensate(context.Background(), Spec{RootID: 1, SQL: downSQL}, nil); err != nil {
		t.Fatal(err)
	}
	if b := balances(t, db); b[100] != rows {
		t.Fatalf("kompensasi ulang tidak boleh mengubah data: %v", b)
	}
}

func TestCompensateRefusesWithoutEvidence(t *testing.T) {
	db := newTarget(t, "batch_unsafe")
	_, err := (&Runner{Pool: db}).Compensate(context.Background(), Spec{RootID: 42, SQL: downSQL}, nil)
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("tanpa bukti eksekusi up harus ditolak: %v", err)
	}
	if b := balances(t, db); b[100] != rows {
		t.Fatalf("data tidak boleh berubah: %v", b)
	}
}

func TestCompensateRefusesNormalModeRunWithoutCheckpoints(t *testing.T) {
	db := newTarget(t, "batch_unsafe_normal")
	r := &Runner{Pool: db}
	if _, err := r.Run(context.Background(), Spec{RootID: 7, Op: OpUp, SQL: `UPDATE perf.accounts SET balance = balance + 1`}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := r.Compensate(context.Background(), Spec{RootID: 7, SQL: downSQL}, nil)
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("run mode biasa tidak punya checkpoint: kompensasi harus ditolak, dapat %v", err)
	}
	var down int
	db.QueryRow(context.Background(), `SELECT count(*) FROM _platform.migration_runs WHERE root_job = 7 AND op = 'down'`).Scan(&down)
	if down != 0 {
		t.Errorf("penolakan tidak boleh meninggalkan catatan run down: %d", down)
	}
	if b := balances(t, db); b[101] != rows {
		t.Errorf("data tidak boleh berubah: %v", b)
	}
}

func TestBatchRunWithoutScopeIsRejectedInsteadOfPanicking(t *testing.T) {
	db := newTarget(t, "batch_noscope")
	r := &Runner{Pool: db}
	if _, err := r.Run(context.Background(), Spec{RootID: 1, Op: OpUp, SQL: `UPDATE perf.accounts SET balance = balance + 1`}, nil); err != nil {
		t.Fatal(err) // membuat tabel kontrol
	}
	// run yang belum selesai dan tidak punya cakupan (bukan buatan mode batch).
	if _, err := db.Exec(context.Background(), `INSERT INTO _platform.migration_runs (root_job, op, total_rows) VALUES (9, 'up', 5)`); err != nil {
		t.Fatal(err)
	}
	_, err := r.Run(context.Background(), batchSpec(9, OpUp, upSQL), nil)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("harus ErrInvalidSpec, bukan panic: %v", err)
	}
	if b := balances(t, db); b[101] != rows {
		t.Errorf("data tidak boleh berubah oleh run yang ditolak: %v", b)
	}
}

func TestInvalidSpecRejected(t *testing.T) {
	r := &Runner{Pool: newTarget(t, "batch_invalid")}
	for name, sp := range map[string]Spec{
		"tanpa sql":      {RootID: 1, Op: OpUp},
		"tanpa root":     {Op: OpUp, SQL: "SELECT 1"},
		"op salah":       {RootID: 1, Op: "x", SQL: "SELECT 1"},
		"tabel injeksi":  {RootID: 1, Op: OpUp, SQL: upSQL, Size: 10, Table: "perf.accounts; DROP TABLE perf.accounts", Column: "id"},
		"tanpa schema":   {RootID: 1, Op: OpUp, SQL: upSQL, Size: 10, Table: "accounts", Column: "id"},
		"kolom injeksi":  {RootID: 1, Op: OpUp, SQL: upSQL, Size: 10, Table: "perf.accounts", Column: "id; --"},
		"ukuran negatif": {RootID: 1, Op: OpUp, SQL: upSQL, Size: -5, Table: "perf.accounts", Column: "id"},
	} {
		if _, err := r.Run(context.Background(), sp, nil); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: harus ErrInvalidSpec, dapat %v", name, err)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	for code, transient := range map[string]bool{"40001": true, "40P01": true, "55P03": true, "42601": false, "23505": false} {
		if got := IsTransient(pg(code)); got != transient {
			t.Errorf("SQLSTATE %s: transient=%v, mau %v", code, got, transient)
		}
		if !IsDefinite(pg(code)) {
			t.Errorf("SQLSTATE %s harus definite", code)
		}
	}
	if IsDefinite(errors.New("koneksi putus")) || IsTransient(errors.New("koneksi putus")) {
		t.Error("error jaringan bukan definite dan harus dibuktikan lewat bukti target, bukan ditebak transient")
	}
	if !IsTransient(ErrBusy) || !IsTransient(ErrRolledBack) || IsTransient(ErrCommitUnknown) {
		t.Error("ErrBusy dan ErrRolledBack aman diulang; ErrCommitUnknown tidak")
	}
}
