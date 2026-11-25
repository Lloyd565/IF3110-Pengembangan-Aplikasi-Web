package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/jobs"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

// Test ini menjalankan worker bersama workflow.Service yang asli (approval leader, history
// attempt, posisi database), bukan fake, untuk membuktikan kontrak kedua modul cocok.

var (
	alice = reqctx.Principal{UserID: 1, Username: "alice"}
	bob   = reqctx.Principal{UserID: 2, Username: "bob"}
	carol = reqctx.Principal{UserID: 3, Username: "carol"}
)

type e2e struct {
	t      *testing.T
	db     *pgxpool.Pool
	target *pgxpool.Pool
	svc    *workflow.Service
	js     *jobs.Service
	w      *Worker
}

func newE2E(t *testing.T, name string) *e2e {
	t.Helper()
	ctx := context.Background()
	db := testdb.Fresh(t, name)
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES (1,'alice','x'),(2,'bob','x'),(3,'carol','x');
		INSERT INTO groups (id, name) VALUES (10,'app'),(20,'ro');
		INSERT INTO group_members VALUES (10,1),(10,3),(20,2);
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc)
			VALUES ('aaaaaaaa-0000-0000-0000-000000000001','A','h',5432,'d','u','\x00');
		INSERT INTO db_grants VALUES (10,'aaaaaaaa-0000-0000-0000-000000000001','edit'),(20,'aaaaaaaa-0000-0000-0000-000000000001','view');
		INSERT INTO group_leaders (group_id, user_id) VALUES (10,1),(20,2);`); err != nil {
		t.Fatal(err)
	}
	az := &authz.Authorizer{DB: db}
	svc := &workflow.Service{DB: db, Authz: az, Queue: jobs.Queue{}}
	target := newTarget(t, name+"_target")
	targets := &env{t: t, platform: db, targets: map[uuid.UUID]*pgxpool.Pool{connA: target}, flow: &fakeFlow{}}
	w := &Worker{DB: db, Flow: svc, Targets: targets, ID: "e2e", Lease: 600 * time.Millisecond, Poll: 10 * time.Millisecond,
		LockWait: 300 * time.Millisecond, ReconcileWait: 300 * time.Millisecond, Backoff: func(int) time.Duration { return 0 }}
	return &e2e{t: t, db: db, target: target, svc: svc, js: &jobs.Service{DB: db, Authz: az, Flow: svc}, w: w}
}

func (e *e2e) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *e2e) save(up, down string) contracts.Migration {
	m, err := e.svc.Save(context.Background(), carol, connA, "", contracts.Draft{Name: "m", SQLUp: up, SQLDown: down})
	e.must(err)
	return m
}

// approved membuat pengajuan lalu menyetujuinya oleh kedua leader (alice untuk group app, bob untuk ro).
func (e *e2e) approved(in contracts.ChangeInput) contracts.ChangeRequest {
	ctx := context.Background()
	x, err := e.svc.Propose(ctx, carol, connA, in)
	e.must(err)
	_, err = e.svc.Submit(ctx, carol, connA, x.ID)
	e.must(err)
	for _, l := range []reqctx.Principal{alice, bob} {
		x, err = e.svc.Review(ctx, l, connA, x.ID, contracts.Review{Decision: "approve"})
		e.must(err)
	}
	return x
}

func batchInput(m contracts.Migration, action, target string) contracts.ChangeInput {
	return contracts.ChangeInput{MigrationID: m.ID, RevisionID: m.RevisionID, Action: action, TargetExecutionID: target,
		Mode: "batch", BatchSize: 100, CursorColumn: "id", Scope: "perf.accounts"}
}

func (e *e2e) enqueue(x contracts.ChangeRequest) contracts.Job {
	j, err := e.svc.Enqueue(context.Background(), alice, connA, x.ID, "key-"+uuid.NewString())
	e.must(err)
	return j
}

func (e *e2e) drain() {
	e.t.Helper()
	for i := 0; i < 50; i++ {
		did, err := e.w.RunOnce(context.Background())
		e.must(err)
		if !did {
			return
		}
	}
	e.t.Fatal("queue tidak kunjung kosong")
}

func (e *e2e) history(hide bool) []contracts.Execution {
	h, err := e.svc.History(context.Background(), alice, connA, hide)
	e.must(err)
	return h
}

func (e *e2e) job(id string) contracts.Job {
	j, err := e.js.Get(context.Background(), alice, connA, id)
	e.must(err)
	return j
}

func (e *e2e) bal() string { return fmt.Sprint(balances(e.t, e.target)) }

func TestEndToEndApplyThenRollbackThroughRealWorkflow(t *testing.T) {
	e := newE2E(t, "e2e_flow")
	m := e.save(upSQL, downSQL)
	x := e.approved(batchInput(m, "apply", ""))
	job := e.enqueue(x)
	if job.Status != "queued" {
		t.Fatalf("API harus langsung mengembalikan job queued (202), dapat %+v", job)
	}
	if e.bal() != "map[100:1000]" {
		t.Fatal("enqueue tidak boleh menjalankan SQL di request")
	}
	// enqueue ulang (retry klien, kunci berbeda) tidak membuat job kedua.
	if again, err := e.svc.Enqueue(context.Background(), alice, connA, x.ID, "key-lain-12345"); err != nil || again.ID != job.ID {
		t.Fatalf("enqueue ulang harus mengembalikan job yang sama: %+v err=%v", again, err)
	}

	e.drain()
	done := e.job(job.ID)
	if done.Status != "success" || done.CommittedBatches != 10 || done.CommittedRows != rows || done.Attempt != 1 {
		t.Fatalf("job: %+v", done)
	}
	if e.bal() != "map[101:1000]" {
		t.Fatalf("balance = %s", e.bal())
	}
	h := e.history(false)
	if len(h) != 1 || h[0].Status != "success" || h[0].Action != "apply" || h[0].SQL != upSQL {
		t.Fatalf("history harus memuat attempt sukses dengan SQL yang dijalankan: %+v", h)
	}

	// rollback melalui jalur yang sama: pengajuan baru, approval, queue, worker.
	rb := e.approved(batchInput(m, "rollback", h[0].ID))
	rbJob := e.enqueue(rb)
	e.drain()
	if got := e.job(rbJob.ID); got.Status != "success" {
		t.Fatalf("rollback job: %+v", got)
	}
	if e.bal() != "map[100:1000]" {
		t.Fatalf("setelah rollback balance = %s", e.bal())
	}
	if full := e.history(false); len(full) != 2 || full[1].RevertsExecutionID != h[0].ID {
		t.Fatalf("history lengkap harus memuat apply dan rollback yang merujuk execution-nya: %+v", full)
	}
	if hidden := e.history(true); len(hidden) != 0 {
		t.Fatalf("Hide rollback menyembunyikan pasangan apply dan rollback: %+v", hidden)
	}
}

// Contoh Hide rollback di spek: apply [E2], rollback [E3] tersembunyi, forward [E4] tetap terlihat.
func TestEndToEndForwardAfterRollbackStaysVisibleWhenPairIsHidden(t *testing.T) {
	e := newE2E(t, "e2e_forward")
	m := e.save(upSQL, downSQL)
	e.enqueue(e.approved(batchInput(m, "apply", "")))
	e.drain()
	applyID := e.history(false)[0].ID
	e.enqueue(e.approved(batchInput(m, "rollback", applyID)))
	e.drain()
	if e.bal() != "map[100:1000]" {
		t.Fatalf("setelah rollback balance = %s", e.bal())
	}
	rollbackID := e.history(false)[1].ID

	// forward menerapkan kembali satu langkah dan tidak menjalankan migration berikutnya.
	fw := e.enqueue(e.approved(batchInput(m, "forward", rollbackID)))
	e.drain()
	if got := e.job(fw.ID); got.Status != "success" || got.CommittedRows != rows {
		t.Fatalf("forward: %+v", got)
	}
	if e.bal() != "map[101:1000]" {
		t.Fatalf("setelah forward balance = %s", e.bal())
	}
	full := e.history(false)
	if len(full) != 3 || full[2].Action != "forward" || full[2].Status != "success" {
		t.Fatalf("history lengkap harus memuat apply, rollback, dan forward: %+v", full)
	}
	hidden := e.history(true)
	if len(hidden) != 1 || hidden[0].ID != full[2].ID {
		t.Fatalf("Hide rollback menyembunyikan pasangan apply dan rollback, forward tetap terlihat: %+v", hidden)
	}
}

func TestEndToEndLeaderChangeBeforeWorkerRunsRejectsJobAndKeepsTargetUntouched(t *testing.T) {
	e := newE2E(t, "e2e_leader")
	m := e.save(upSQL, downSQL)
	job := e.enqueue(e.approved(batchInput(m, "apply", "")))
	// leader group app diganti setelah approval dan sebelum worker berjalan.
	if _, err := e.db.Exec(context.Background(), `UPDATE group_leaders SET user_id = 3 WHERE group_id = 10`); err != nil {
		t.Fatal(err)
	}
	e.drain()
	got := e.job(job.ID)
	if got.Status != "failed" || got.CommittedRows != 0 {
		t.Fatalf("approval lama tidak boleh dipakai: %+v", got)
	}
	if e.bal() != "map[100:1000]" {
		t.Fatalf("target tidak boleh tersentuh: %s", e.bal())
	}
	h := e.history(false)
	if len(h) != 1 || h[0].Status != "failed" {
		t.Fatalf("penolakan revalidasi harus tercatat sebagai attempt gagal: %+v", h)
	}
}

func TestEndToEndPartialBlocksOthersThenCompensationNeedsNewApproval(t *testing.T) {
	e := newE2E(t, "e2e_comp")
	ctx := context.Background()
	m := e.save(upSQL, downSQL)
	e.w.Hooks.BeforeCommit = func(op string, n int) error {
		if op == "up" && n == 4 {
			return errors.New("gangguan terus-menerus")
		}
		return nil
	}
	job := e.enqueue(e.approved(batchInput(m, "apply", "")))
	e.drain() // retry habis: partial
	got := e.job(job.ID)
	if got.Status != "partial" || got.CommittedBatches != 3 || !got.CanCompensate || !got.CanResume {
		t.Fatalf("job harus partial dengan 3 batch: %+v", got)
	}
	h := e.history(false)
	if h[len(h)-1].Status != "partial" {
		t.Fatalf("history mencatat attempt partial: %+v", h)
	}

	// partial menahan migration lain.
	other := e.save("SELECT 1", "SELECT 2")
	blocked, err := e.svc.Propose(ctx, carol, connA, contracts.ChangeInput{MigrationID: other.ID, RevisionID: other.RevisionID, Action: "apply", Mode: "normal"})
	e.must(err)
	if len(blocked.Validation) == 0 {
		t.Fatalf("migration lain harus ditahan selama ada partial: %+v", blocked)
	}

	// kompensasi: pengajuan rollback baru, wajib disetujui lagi oleh seluruh leader.
	req, err := e.js.Recover(ctx, carol, connA, job.ID, "compensate", "k-compensate-1")
	e.must(err)
	if req.Action != "rollback" || req.TargetExecutionID == "" || req.Mode != "batch" {
		t.Fatalf("pengajuan kompensasi: %+v", req)
	}
	// klik ganda: pengajuan yang masih terbuka dikembalikan, tidak ada draf kedua.
	if again, err := e.js.Recover(ctx, carol, connA, job.ID, "compensate", "k-compensate-2"); err != nil || again.ID != req.ID {
		t.Fatalf("kompensasi kedua harus mengembalikan pengajuan yang sama: %+v err=%v", again, err)
	}
	var drafts int
	e.must(e.db.QueryRow(ctx, `SELECT count(*) FROM migration_requests WHERE action = 'rollback' AND target_execution_id = $1::uuid`, req.TargetExecutionID).Scan(&drafts))
	if drafts != 1 {
		t.Fatalf("pengajuan kompensasi = %d, mau 1", drafts)
	}
	if _, err := e.svc.Enqueue(ctx, alice, connA, req.ID, "key-tanpa-approval"); err == nil {
		t.Fatal("kompensasi tidak boleh masuk queue sebelum disetujui")
	}
	_, err = e.svc.Submit(ctx, carol, connA, req.ID)
	e.must(err)
	for _, l := range []reqctx.Principal{alice, bob} {
		req, err = e.svc.Review(ctx, l, connA, req.ID, contracts.Review{Decision: "approve"})
		e.must(err)
	}
	if req.Status != "approved" || len(req.Validation) != 0 {
		t.Fatalf("setelah approval kompensasi harus bisa di-enqueue: %+v", req)
	}
	e.w.Hooks.BeforeCommit = nil
	compJob := e.enqueue(req)
	var kind string
	e.must(e.db.QueryRow(ctx, `SELECT kind FROM jobs WHERE id = $1::bigint`, compJob.ID).Scan(&kind))
	if kind != "compensate" {
		t.Fatalf("job kompensasi kind = %s", kind)
	}
	e.drain()
	if c := e.job(compJob.ID); c.Status != "success" {
		t.Fatalf("job kompensasi: %+v", c)
	}
	if e.bal() != "map[100:1000]" {
		t.Fatalf("seluruh batch yang committed harus dikembalikan: %s", e.bal())
	}
	// penahan terlepas: migration lain bisa diajukan lagi.
	free, err := e.svc.Propose(ctx, carol, connA, contracts.ChangeInput{MigrationID: other.ID, RevisionID: other.RevisionID, Action: "apply", Mode: "normal"})
	e.must(err)
	if len(free.Validation) != 0 {
		t.Fatalf("setelah kompensasi selesai migration lain tidak boleh tertahan: %+v", free.Validation)
	}
}

func TestEndToEndResumeFinishesRemainingBatchesOnly(t *testing.T) {
	e := newE2E(t, "e2e_resume")
	ctx := context.Background()
	m := e.save(upSQL, downSQL)
	e.w.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 4 {
			return errors.New("gangguan terus-menerus")
		}
		return nil
	}
	job := e.enqueue(e.approved(batchInput(m, "apply", "")))
	e.drain()
	if got := e.job(job.ID); got.Status != "partial" {
		t.Fatalf("job: %+v", got)
	}
	e.w.Hooks.BeforeCommit = nil
	var seen []int
	e.w.Hooks.AfterCommit = func(op string, n int) error { seen = append(seen, n); return nil }

	if _, err := e.js.Recover(ctx, bob, connA, job.ID, "resume", "k-resume-1"); err == nil {
		t.Fatal("bob hanya View: tidak boleh memulihkan")
	}
	req, err := e.js.Recover(ctx, carol, connA, job.ID, "resume", "k-resume-1")
	e.must(err)
	if req.Action != "apply" {
		t.Fatalf("resume memakai pengajuan semula: %+v", req)
	}
	if _, err := e.js.Recover(ctx, carol, connA, job.ID, "resume", "k-resume-2"); err == nil {
		t.Fatal("resume kedua selagi yang pertama masih berjalan harus 409")
	}
	e.drain()
	if len(seen) != 7 || seen[0] != 4 {
		t.Fatalf("resume hanya boleh memproses batch 4-10: %v", seen)
	}
	if e.bal() != "map[101:1000]" {
		t.Fatalf("tidak boleh ada double commit: %s", e.bal())
	}
	h := e.history(false)
	if h[len(h)-1].Status != "success" {
		t.Fatalf("attempt resume yang sukses menjadi attempt terakhir: %+v", h)
	}
	// penahan terlepas setelah resume berhasil.
	other := e.save("SELECT 1", "SELECT 2")
	free, err := e.svc.Propose(ctx, carol, connA, contracts.ChangeInput{MigrationID: other.ID, RevisionID: other.RevisionID, Action: "apply", Mode: "normal"})
	e.must(err)
	if len(free.Validation) != 0 {
		t.Fatalf("setelah resume selesai migration lain tidak boleh tertahan: %+v", free.Validation)
	}
}

func TestEndToEndJobsAPIScopesToConnection(t *testing.T) {
	e := newE2E(t, "e2e_scope")
	m := e.save(upSQL, downSQL)
	job := e.enqueue(e.approved(batchInput(m, "apply", "")))
	list, err := e.js.List(context.Background(), alice, connA)
	if err != nil || len(list) != 1 || list[0].ID != job.ID {
		t.Fatalf("list: %+v err=%v", list, err)
	}
	other := uuid.New()
	_, err = e.js.Get(context.Background(), alice, other, job.ID)
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("job milik koneksi lain tidak boleh terlihat: %v", err)
	}
	if _, err := e.js.Get(context.Background(), alice, connA, "abc"); !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("ID tidak valid harus 404: %v", err)
	}
}
