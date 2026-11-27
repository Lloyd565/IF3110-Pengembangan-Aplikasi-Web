package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	if len(h) != 1 || h[0].Status != "success" || h[0].Action != "apply" ||
		!strings.Contains(h[0].SQL, "RETURNING accounts.id, accounts.xmin::text, accounts.ctid::text") {
		t.Fatalf("history harus memuat attempt sukses dengan SQL yang dijalankan: %+v", h)
	}

	rb := e.approved(batchInput(m, "rollback", h[0].ID))
	rbJob := e.enqueue(rb)
	e.drain()
	if got := e.job(rbJob.ID); got.Status != "success" {
		t.Fatalf("rollback job: %+v", got)
	}
	if e.bal() != "map[100:1000]" {
		t.Fatalf("setelah rollback balance = %s", e.bal())
	}
	if full := e.history(false); len(full) != 2 || full[1].RevertsExecutionID != h[0].ID ||
		!strings.Contains(full[1].SQL, "accounts.id = ANY(") {
		t.Fatalf("history lengkap harus memuat apply dan rollback yang merujuk execution-nya: %+v", full)
	}
	if hidden := e.history(true); len(hidden) != 0 {
		t.Fatalf("Hide rollback menyembunyikan pasangan apply dan rollback: %+v", hidden)
	}
}

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

	ctx := context.Background()
	stale, err := e.svc.Propose(ctx, carol, connA, contracts.ChangeInput{
		MigrationID: m.ID, RevisionID: m.RevisionID, Action: "forward", TargetExecutionID: rollbackID,
	})
	e.must(err)
	_, err = e.db.Exec(ctx, `UPDATE migration_requests SET mode='normal',batch_size=0,cursor_column='',scope='' WHERE id=$1::uuid`, stale.ID)
	e.must(err)
	stale, err = e.svc.Request(ctx, carol, connA, stale.ID)
	e.must(err)
	if stale.CanSubmit || len(stale.Validation) == 0 {
		t.Fatalf("legacy draft must explain why a fresh proposal is required: %+v", stale)
	}

	proposal := e.approved(contracts.ChangeInput{
		MigrationID: m.ID, RevisionID: m.RevisionID, Action: "forward", TargetExecutionID: rollbackID, Mode: "normal",
	})
	if proposal.Mode != "batch" || proposal.BatchSize != 100 || proposal.CursorColumn != "id" || proposal.Scope != "perf.accounts" {
		t.Fatalf("forward should inherit the original batch configuration: %+v", proposal)
	}
	fw := e.enqueue(proposal)
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

func TestEndToEndFilteredBatchReportsActualTotal(t *testing.T) {
	e := newE2E(t, "e2e_filtered_batch")
	m := e.save(upSQL+" AND id % 2 = 0", downSQL+" AND id % 2 = 0")
	job := e.enqueue(e.approved(batchInput(m, "apply", "")))
	e.drain()

	got := e.job(job.ID)
	if got.Status != "success" || got.CommittedRows != rows/2 || got.TotalRows == nil || *got.TotalRows != got.CommittedRows {
		t.Fatalf("finished job should report the actual affected row count: %+v", got)
	}
	if e.bal() != "map[100:500 101:500]" {
		t.Fatalf("expected half the rows to change: %s", e.bal())
	}
}

func TestEndToEndHistoryDistinguishesPlanFromDispatchedSQL(t *testing.T) {
	e := newE2E(t, "e2e_sql_dispatch")
	_, err := e.target.Exec(context.Background(), "TRUNCATE perf.accounts")
	e.must(err)
	empty := e.save(upSQL, downSQL)
	job := e.enqueue(e.approved(batchInput(empty, "apply", "")))
	e.drain()
	if got := e.job(job.ID); got.Status != "success" || got.CommittedBatches != 0 {
		t.Fatalf("empty batch job: %+v", got)
	}
	h := e.history(false)
	if len(h) != 1 || h[0].SQL != "" || h[0].SQLSource != "not_dispatched" || h[0].PlannedSQL != upSQL {
		t.Fatalf("empty batch should show the plan separately: %+v", h)
	}

	failed := e.save("SELECT 1 / 0", "SELECT 1")
	job = e.enqueue(e.approved(contracts.ChangeInput{
		MigrationID: failed.ID, RevisionID: failed.RevisionID, Action: "apply", Mode: "normal",
	}))
	e.drain()
	h = e.history(false)
	if len(h) != 2 || h[1].Status != "failed" || h[1].SQL != "SELECT 1 / 0" || h[1].SQLSource != "dispatched" ||
		h[1].ID != e.job(job.ID).ExecutionID {
		t.Fatalf("failed statement should be recorded as dispatched: %+v", h)
	}
}

func TestEndToEndLeaderChangeBeforeWorkerRunsRejectsJobAndKeepsTargetUntouched(t *testing.T) {
	e := newE2E(t, "e2e_leader")
	m := e.save(upSQL, downSQL)
	job := e.enqueue(e.approved(batchInput(m, "apply", "")))

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
	e.drain()
	got := e.job(job.ID)
	if got.Status != "partial" || got.CommittedBatches != 3 || !got.CanCompensate || !got.CanResume {
		t.Fatalf("job harus partial dengan 3 batch: %+v", got)
	}
	h := e.history(false)
	if h[len(h)-1].Status != "partial" {
		t.Fatalf("history mencatat attempt partial: %+v", h)
	}

	other := e.save("SELECT 1", "SELECT 2")
	blocked, err := e.svc.Propose(ctx, carol, connA, contracts.ChangeInput{MigrationID: other.ID, RevisionID: other.RevisionID, Action: "apply", Mode: "normal"})
	e.must(err)
	if len(blocked.Validation) == 0 {
		t.Fatalf("migration lain harus ditahan selama ada partial: %+v", blocked)
	}

	req, err := e.js.Recover(ctx, carol, connA, job.ID, "compensate", "k-compensate-1")
	e.must(err)
	if req.Action != "rollback" || req.TargetExecutionID == "" || req.Mode != "batch" {
		t.Fatalf("pengajuan kompensasi: %+v", req)
	}

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
	if h[0].Actor != alice.Username || h[len(h)-1].Status != "success" || h[len(h)-1].Actor != carol.Username {
		t.Fatalf("history must retain the original actor and credit the resumer: %+v", h)
	}

	other := e.save("SELECT 1", "SELECT 2")
	free, err := e.svc.Propose(ctx, carol, connA, contracts.ChangeInput{MigrationID: other.ID, RevisionID: other.RevisionID, Action: "apply", Mode: "normal"})
	e.must(err)
	if len(free.Validation) != 0 {
		t.Fatalf("setelah resume selesai migration lain tidak boleh tertahan: %+v", free.Validation)
	}
}

func (e *e2e) submitApproved(id string) contracts.ChangeRequest {
	ctx := context.Background()
	_, err := e.svc.Submit(ctx, carol, connA, id)
	e.must(err)
	var x contracts.ChangeRequest
	for _, l := range []reqctx.Principal{alice, bob} {
		x, err = e.svc.Review(ctx, l, connA, id, contracts.Review{Decision: "approve"})
		e.must(err)
	}
	return x
}

func TestEndToEndCompensationStuckInCommitUnknownIsRecoverable(t *testing.T) {
	e := newE2E(t, "e2e_comp_unknown")
	ctx := context.Background()
	m := e.save(upSQL, downSQL)
	e.w.Hooks.BeforeCommit = func(op string, n int) error {
		if op == "up" && n == 4 {
			return errors.New("gangguan terus-menerus")
		}
		return nil
	}
	apply := e.enqueue(e.approved(batchInput(m, "apply", "")))
	e.drain()
	if got := e.job(apply.ID); got.Status != "partial" || got.CommittedBatches != 3 {
		t.Fatalf("prasyarat: apply harus partial 3 batch: %+v", got)
	}

	req, err := e.js.Recover(ctx, carol, connA, apply.ID, "compensate", "k-comp-1")
	e.must(err)
	req = e.submitApproved(req.ID)

	var holder *pgxpool.Conn
	var released bool
	release := func() {
		if holder != nil && !released {
			released = true
			holder.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('if3110.migration'))`)
			holder.Release()
		}
	}
	e.w.Hooks.BeforeCommit = func(op string, n int) error {
		if op == "down" && n == 2 && holder == nil {
			e.target.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid() AND state = 'idle in transaction'`)
			time.Sleep(150 * time.Millisecond)
			var err error
			if holder, err = e.target.Acquire(ctx); err != nil {
				t.Fatal(err)
			}
			holder.Exec(ctx, `SELECT pg_advisory_lock(hashtext('if3110.migration'))`)

			t.Cleanup(release)
		}
		return nil
	}
	comp := e.enqueue(req)
	e.drain()
	if holder == nil {
		t.Fatal("prasyarat: kegagalan koneksi tidak terpicu")
	}
	stuck := e.job(comp.ID)
	if stuck.Status != "commit_unknown" || stuck.CanResume || !stuck.CanCompensate {
		t.Fatalf("kompensasi commit_unknown harus punya jalur kompensasi lanjutan: %+v", stuck)
	}

	release()
	e.w.Hooks.BeforeCommit = nil
	next, err := e.js.Recover(ctx, carol, connA, comp.ID, "compensate", "k-comp-2")
	e.must(err)
	if next.TargetExecutionID != stuck.ExecutionID {
		t.Fatalf("kompensasi lanjutan harus menargetkan attempt yang tertahan: %s, mau %s", next.TargetExecutionID, stuck.ExecutionID)
	}
	if _, err := e.svc.Enqueue(ctx, alice, connA, next.ID, "key-tanpa-approval-2"); err == nil {
		t.Fatal("kompensasi lanjutan tidak boleh masuk queue sebelum disetujui")
	}
	next = e.submitApproved(next.ID)
	if next.Status != "approved" || len(next.Validation) != 0 {
		t.Fatalf("setelah approval kompensasi lanjutan harus bisa di-enqueue: %+v", next)
	}
	cont := e.enqueue(next)
	var kind string
	var root, applyRoot int64
	e.must(e.db.QueryRow(ctx, `SELECT kind, root_job_id FROM jobs WHERE id = $1::bigint`, cont.ID).Scan(&kind, &root))
	e.must(e.db.QueryRow(ctx, `SELECT root_job_id FROM jobs WHERE id = $1::bigint`, apply.ID).Scan(&applyRoot))
	if kind != "compensate" || root != applyRoot {
		t.Fatalf("job lanjutan harus kompensasi pada root apply yang sama: kind=%s root=%d mau %d", kind, root, applyRoot)
	}
	e.drain()
	if got := e.job(cont.ID); got.Status != "success" {
		t.Fatalf("kompensasi lanjutan: %+v", got)
	}
	if e.bal() != "map[100:1000]" {
		t.Fatalf("seluruh batch harus kembali tepat sekali: %s", e.bal())
	}
	other := e.save("SELECT 1", "SELECT 2")
	free, err := e.svc.Propose(ctx, carol, connA, contracts.ChangeInput{MigrationID: other.ID, RevisionID: other.RevisionID, Action: "apply", Mode: "normal"})
	e.must(err)
	if len(free.Validation) != 0 {
		t.Fatalf("setelah pemulihan selesai migration lain tidak boleh tertahan: %+v", free.Validation)
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
