package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/batch"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/jobs"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

const (
	upSQL   = `UPDATE perf.accounts SET balance = balance + 1 WHERE id > $1 AND id <= $2`
	downSQL = `UPDATE perf.accounts SET balance = balance - 1 WHERE id > $1 AND id <= $2`
	rows    = 1000
)

var (
	connA = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	connB = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
)

type attempt struct {
	ID, JobID, Status, Err string
	No                     int
	Plan                   workflow.Plan
}

// fakeFlow menggantikan workflow milik Richard (approval dan history) pada test worker.
type fakeFlow struct {
	mu                  sync.Mutex
	plan                workflow.Plan
	prepareErr          error
	prepared, recovered int
	attempts            []*attempt
}

func (f *fakeFlow) Prepare(ctx context.Context, rid string) (workflow.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared++
	if f.prepareErr != nil {
		return workflow.Plan{}, f.prepareErr
	}
	return f.plan, nil
}

func (f *fakeFlow) PrepareRecovery(ctx context.Context, rid string, _ int64) (workflow.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovered++
	return f.plan, nil
}

func (f *fakeFlow) BeginAttempt(ctx context.Context, pl workflow.Plan, jobID string, no int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &attempt{ID: uuid.NewString(), JobID: jobID, No: no, Status: "running", Plan: pl}
	f.attempts = append(f.attempts, a)
	return a.ID, nil
}

func (f *fakeFlow) FinishAttempt(ctx context.Context, id, status, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.attempts {
		if a.ID == id {
			if (a.Status == "success" || a.Status == "failed") && a.Status != status {
				return &contracts.Error{Status: 409, Code: "conflict", Message: "attempt sudah final"}
			}
			a.Status, a.Err = status, msg
			return nil
		}
	}
	return fmt.Errorf("attempt %s tidak ada", id)
}

func (f *fakeFlow) statuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.attempts {
		out = append(out, a.Status)
	}
	return out
}

type env struct {
	t        *testing.T
	platform *pgxpool.Pool
	targets  map[uuid.UUID]*pgxpool.Pool
	flow     *fakeFlow
}

func (e *env) Target(ctx context.Context, id uuid.UUID) (*dbconn.Target, error) {
	return &dbconn.Target{DBMS: dbconn.PostgreSQL, Postgres: e.targets[id]}, nil
}

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

func newEnv(t *testing.T, name string) *env {
	t.Helper()
	ctx := context.Background()
	platform := testdb.Fresh(t, name+"_platform")
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Exec(ctx, `INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc) VALUES
		($1, 'A', 'h', 5432, 'd', 'u', '\x00'), ($2, 'B', 'h', 5432, 'd', 'u', '\x00')`, connA, connB); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, platform: platform, flow: &fakeFlow{}, targets: map[uuid.UUID]*pgxpool.Pool{
		connA: newTarget(t, name+"_a"), connB: newTarget(t, name+"_b"),
	}}
}

func (e *env) worker(id string) *Worker {
	return &Worker{DB: e.platform, Flow: e.flow, Targets: e, ID: id, Lease: 600 * time.Millisecond, Poll: 10 * time.Millisecond,
		LockWait: 300 * time.Millisecond, ReconcileWait: 300 * time.Millisecond, Backoff: func(int) time.Duration { return 0 }}
}

func batchPlan(sql string) workflow.Plan {
	return workflow.Plan{RequestID: uuid.NewString(), MigrationID: "1", Action: "apply", Mode: "batch", BatchSize: 100,
		CursorColumn: "id", Scope: "perf.accounts", SQL: sql, ActorID: 1}
}

func normalPlan(sql string) workflow.Plan {
	return workflow.Plan{RequestID: uuid.NewString(), MigrationID: "1", Action: "apply", Mode: "normal", SQL: sql, ActorID: 1}
}

type opt func(*jobs.NewJob)

func kind(k string, parent int64) opt {
	return func(n *jobs.NewJob) { n.Kind, n.ParentJobID = k, &parent }
}
func retries(n int) opt { return func(j *jobs.NewJob) { j.MaxRetries = n } }

func (e *env) enqueue(conn uuid.UUID, plan workflow.Plan, opts ...opt) int64 {
	e.t.Helper()
	plan.ConnectionID = conn
	e.flow.plan = plan
	raw, _ := json.Marshal(plan)
	in := jobs.NewJob{ConnectionID: conn, RequestID: uuid.MustParse(plan.RequestID), Key: uuid.NewString(), Plan: raw}
	for _, o := range opts {
		o(&in)
	}
	id, _, err := jobs.Enqueue(context.Background(), e.platform, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// drain menjalankan job sampai queue kosong (retry langsung siap karena backoff 0).
func (e *env) drain(w *Worker) int {
	e.t.Helper()
	n := 0
	for i := 0; i < 50; i++ {
		did, err := w.RunOnce(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if !did {
			return n
		}
		n++
	}
	e.t.Fatal("queue tidak kunjung kosong")
	return n
}

func (e *env) job(id int64) *jobs.Job {
	e.t.Helper()
	j, err := jobs.Get(context.Background(), e.platform, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

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

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, mau %v", what, got, want)
	}
}

func TestNormalModeJobRunsOnceAndRecordsAttempt(t *testing.T) {
	e := newEnv(t, "w_normal")
	id := e.enqueue(connA, normalPlan(`UPDATE perf.accounts SET balance = balance + 1`))
	e.drain(e.worker("w1"))
	j := e.job(id)
	eq(t, "status job", j.Status, "success")
	eq(t, "attempt", e.flow.statuses(), "[success]")
	eq(t, "balance", balances(t, e.targets[connA]), "map[101:1000]")
	if j.Attempt != 1 || j.ExecutionID == "" || j.AttemptOpen || j.CommittedRows != rows {
		t.Errorf("job tidak lengkap: %+v", j)
	}
	if e.flow.prepared != 1 {
		t.Errorf("revalidasi harus berjalan sekali sebelum eksekusi, prepared=%d", e.flow.prepared)
	}
	if got := e.flow.attempts[0].Plan.SQL; got == "" {
		t.Error("history harus menyimpan SQL yang dijalankan")
	}
}

func TestBatchModeReportsProgress(t *testing.T) {
	e := newEnv(t, "w_batch")
	id := e.enqueue(connA, batchPlan(upSQL))
	e.drain(e.worker("w1"))
	j := e.job(id)
	eq(t, "status", jobs.Public(j).Status, "success")
	if j.CommittedBatches != 10 || j.CommittedRows != rows || j.TotalRows == nil || *j.TotalRows != rows {
		t.Errorf("progress: batches=%d rows=%d total=%v", j.CommittedBatches, j.CommittedRows, j.TotalRows)
	}
	eq(t, "balance", balances(t, e.targets[connA]), "map[101:1000]")
}

func TestPermanentSQLErrorFailsWithoutRetry(t *testing.T) {
	e := newEnv(t, "w_syntax")
	id := e.enqueue(connA, normalPlan(`UPDATE perf.accounts SET tidak_ada = 1`))
	e.drain(e.worker("w1"))
	j := e.job(id)
	if j.Status != "failed" || j.Hold != "none" || j.RetryCount != 0 {
		t.Errorf("SQL salah harus gagal permanen tanpa retry: %+v", j)
	}
	eq(t, "attempt", e.flow.statuses(), "[failed]")
	eq(t, "balance", balances(t, e.targets[connA]), "map[100:1000]")
}

func TestBatchFailureMidwayBecomesPartialThenResumesOnRetry(t *testing.T) {
	e := newEnv(t, "w_retry")
	id := e.enqueue(connA, batchPlan(upSQL))
	w := e.worker("w1")
	failed := false
	w.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 4 && !failed {
			failed = true
			return errors.New("gangguan sementara")
		}
		return nil
	}
	e.drain(w)
	j := e.job(id)
	eq(t, "status", j.Status, "success")
	eq(t, "retry_count", j.RetryCount, 1)
	// setiap attempt tercatat: yang gagal tetap terlihat sebagai partial, tidak ditimpa oleh yang sukses.
	eq(t, "attempts", e.flow.statuses(), "[partial success]")
	eq(t, "nomor attempt", fmt.Sprint(e.flow.attempts[0].No, e.flow.attempts[1].No), "1 2")
	eq(t, "balance", balances(t, e.targets[connA]), "map[101:1000]")
	if j.CommittedBatches != 10 {
		t.Errorf("batches = %d, mau 10", j.CommittedBatches)
	}
}

func TestRetriesExhaustedLeavesPartialJobThatCanBeResumedOrCompensated(t *testing.T) {
	e := newEnv(t, "w_exhaust")
	id := e.enqueue(connA, batchPlan(upSQL), retries(1))
	w := e.worker("w1")
	w.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 4 {
			return errors.New("gangguan terus-menerus")
		}
		return nil
	}
	e.drain(w)
	j := e.job(id)
	if j.Status != "failed" || j.Hold != "partial" || j.CommittedBatches != 3 {
		t.Fatalf("job harus failed dengan hold partial dan 3 batch committed: %+v", j)
	}
	pub := jobs.Public(j)
	if pub.Status != "partial" || !pub.CanResume || !pub.CanCompensate {
		t.Errorf("API: %+v", pub)
	}
	eq(t, "attempts", e.flow.statuses(), "[partial partial]")

	// resume: hanya batch yang belum committed; revalidasi memakai jalur pemulihan.
	w.Hooks.BeforeCommit = nil
	resume := e.enqueue(connA, batchPlan(upSQL), kind(jobs.KindResume, id))
	e.flow.plan = e.flow.attempts[0].Plan // pengajuan semula yang sama
	e.drain(w)
	r := e.job(resume)
	if r.Status != "success" || r.RootID != id || e.flow.recovered < 1 {
		t.Fatalf("resume: %+v recovered=%d", r, e.flow.recovered)
	}
	eq(t, "balance", balances(t, e.targets[connA]), "map[101:1000]")
}

func TestDuplicateDeliveryDoesNotDoubleCommit(t *testing.T) {
	e := newEnv(t, "w_dup")
	id := e.enqueue(connA, batchPlan(upSQL))
	w := e.worker("w1")
	e.drain(w)
	// pesan job terkirim ulang setelah selesai.
	if _, err := e.platform.Exec(context.Background(), `UPDATE jobs SET status = 'queued', worker_id = NULL, lease_until = NULL, finished_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	e.drain(w)
	eq(t, "status", e.job(id).Status, "success")
	eq(t, "balance", balances(t, e.targets[connA]), "map[101:1000]")
	eq(t, "attempts", e.flow.statuses(), "[success success]")
}

func TestCrashedWorkerJobIsTakenOverWithoutRepeatingBatches(t *testing.T) {
	e := newEnv(t, "w_crash")
	id := e.enqueue(connA, batchPlan(upSQL))
	// worker pertama mati setelah 3 batch committed dan attempt-nya masih terbuka.
	dead := &batch.Runner{Pool: e.targets[connA]}
	dead.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 4 {
			return errors.New("proses mati")
		}
		return nil
	}
	root := e.job(id).RootID
	if _, err := dead.Run(context.Background(), batch.Spec{RootID: root, Op: batch.OpUp, SQL: upSQL, Size: 100, Table: "perf.accounts", Column: "id"}, nil); err == nil {
		t.Fatal("harus gagal")
	}
	execID, _ := e.flow.BeginAttempt(context.Background(), e.flow.plan, fmt.Sprint(id), 1)
	if _, err := e.platform.Exec(context.Background(), `UPDATE jobs SET status = 'running', worker_id = 'mati', lease_until = now() - interval '1 second',
		attempt = 1, attempt_open = true, execution_id = $2, committed_batches = 3, committed_rows = 300 WHERE id = $1`, id, execID); err != nil {
		t.Fatal(err)
	}
	var seen []int
	w := e.worker("pengganti")
	w.Hooks.AfterCommit = func(op string, n int) error { seen = append(seen, n); return nil }
	e.drain(w)
	j := e.job(id)
	if j.Status != "success" || !strings.HasPrefix(j.WorkerID, "pengganti:") || j.Attempt != 2 {
		t.Fatalf("worker pengganti harus menyelesaikan: %+v", j)
	}
	if _, err := uuid.Parse(strings.TrimPrefix(j.WorkerID, "pengganti:")); err != nil {
		t.Fatalf("klaim worker harus memiliki token UUID: %v", err)
	}
	eq(t, "attempts", e.flow.statuses(), "[failed success]")
	if e.flow.attempts[0].Err == "" {
		t.Error("attempt yatim harus ditutup dengan alasan")
	}
	if len(seen) != 7 || seen[0] != 4 {
		t.Errorf("hanya batch 4-10 yang boleh dijalankan: %v", seen)
	}
	eq(t, "balance", balances(t, e.targets[connA]), "map[101:1000]")
}

func TestWorkerThatLosesLeaseStopsAndDoesNotWriteResults(t *testing.T) {
	e := newEnv(t, "w_lease")
	id := e.enqueue(connA, batchPlan(upSQL))
	w := e.worker("lama")
	reached, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w.Hooks.BeforeCommit = func(op string, n int) error {
		if n == 2 {
			once.Do(func() { close(reached) })
			<-proceed
		}
		return nil
	}
	done := make(chan struct{})
	go func() { defer close(done); w.RunOnce(context.Background()) }()
	<-reached
	// worker lain mengambil alih job selagi worker lama masih berjalan.
	if _, err := e.platform.Exec(context.Background(), `UPDATE jobs SET worker_id = 'pengambil', lease_until = now() + interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond) // heartbeat (lease/3) mendeteksi kehilangan kepemilikan
	close(proceed)
	<-done
	j := e.job(id)
	if j.WorkerID != "pengambil" || j.Status != "running" {
		t.Errorf("worker lama tidak boleh mengubah job yang sudah diambil alih: %+v", j)
	}
	eq(t, "attempt", e.flow.statuses(), "[running]")
	var cps int
	e.targets[connA].QueryRow(context.Background(), `SELECT count(*) FROM _platform.migration_checkpoints`).Scan(&cps)
	if cps > 2 {
		t.Errorf("worker lama harus berhenti segera setelah kehilangan lease, checkpoint=%d", cps)
	}
}

func TestRevalidationRejectionIsRecordedAndSkipsTarget(t *testing.T) {
	e := newEnv(t, "w_reject")
	id := e.enqueue(connA, normalPlan(`UPDATE perf.accounts SET balance = 0`))
	e.flow.prepareErr = &contracts.Error{Status: 409, Code: "conflict", Message: "Approval tidak lagi sah"}
	e.drain(e.worker("w1"))
	j := e.job(id)
	if j.Status != "failed" || j.Hold != "none" || j.RetryCount != 0 {
		t.Errorf("penolakan revalidasi harus gagal permanen: %+v", j)
	}
	eq(t, "attempt", e.flow.statuses(), "[failed]")
	if e.flow.attempts[0].Err == "" {
		t.Error("alasan penolakan harus tercatat di history")
	}
	eq(t, "balance", balances(t, e.targets[connA]), "map[100:1000]")
	var exists bool
	e.targets[connA].QueryRow(context.Background(), `SELECT to_regclass('_platform.migration_runs') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Error("target tidak boleh disentuh sebelum revalidasi lolos")
	}
}

func TestTransientRevalidationFailureRetries(t *testing.T) {
	e := newEnv(t, "w_prep")
	id := e.enqueue(connA, normalPlan(`UPDATE perf.accounts SET balance = balance + 1`))
	e.flow.prepareErr = errors.New("database platform tidak terjangkau")
	w := e.worker("w1")
	if did, err := w.RunOnce(context.Background()); !did || err != nil {
		t.Fatal(did, err)
	}
	j := e.job(id)
	if j.Status != "retry_wait" || len(e.flow.attempts) != 0 {
		t.Fatalf("gangguan sementara harus retry tanpa mencatat attempt: %+v attempts=%d", j, len(e.flow.attempts))
	}
	e.flow.prepareErr = nil
	e.drain(w)
	eq(t, "status", e.job(id).Status, "success")
	eq(t, "balance", balances(t, e.targets[connA]), "map[101:1000]")
}

func TestMultipleWorkersSerializePerDatabaseAndNeverDoubleCommit(t *testing.T) {
	e := newEnv(t, "w_multi")
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, e.enqueue(connA, normalPlan(`UPDATE perf.accounts SET balance = balance + 1`)))
		ids = append(ids, e.enqueue(connB, normalPlan(`UPDATE perf.accounts SET balance = balance + 1`)))
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		w := e.worker(fmt.Sprintf("w%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 400; k++ {
				did, err := w.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				var open int
				e.platform.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE status NOT IN ('success','failed','cancelled')`).Scan(&open)
				if !did && open == 0 {
					return
				}
				if !did {
					time.Sleep(20 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if s := e.job(id).Status; s != "success" {
			t.Fatalf("job %d status %s", id, s)
		}
	}
	for _, conn := range []uuid.UUID{connA, connB} {
		eq(t, "balance "+conn.String()[:4], balances(t, e.targets[conn]), "map[103:1000]")
	}
	// pada database yang sama, job berjalan bergantian: job berikut baru mulai setelah job sebelumnya selesai.
	type span struct{ start, end time.Time }
	for _, conn := range []uuid.UUID{connA, connB} {
		rs, _ := e.platform.Query(context.Background(), `SELECT started_at, finished_at FROM jobs WHERE connection_id = $1 ORDER BY started_at`, conn)
		var spans []span
		for rs.Next() {
			var s span
			rs.Scan(&s.start, &s.end)
			spans = append(spans, s)
		}
		rs.Close()
		sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
		for i := 1; i < len(spans); i++ {
			if spans[i].start.Before(spans[i-1].end) {
				t.Errorf("job pada database yang sama berjalan tumpang tindih: %v dan %v", spans[i-1], spans[i])
			}
		}
	}
}

func TestCommitUnknownHoldsUntilEvidenceThenResumeReconciles(t *testing.T) {
	e := newEnv(t, "w_unknown")
	target := e.targets[connA]
	id := e.enqueue(connA, batchPlan(upSQL), retries(1))
	w := e.worker("w1")
	w.LockWait = 100 * time.Millisecond
	w.ReconcileWait = 100 * time.Millisecond
	var holder *pgxpool.Conn
	var once sync.Once
	w.Hooks.BeforeCommit = func(op string, n int) error {
		if n != 2 {
			return nil
		}
		once.Do(func() {
			// koneksi worker diputus dan target tidak bisa dikunci: hasil commit tidak bisa dipastikan.
			target.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid() AND state = 'idle in transaction'`)
			time.Sleep(150 * time.Millisecond)
			holder, _ = target.Acquire(context.Background())
			holder.Exec(context.Background(), `SELECT pg_advisory_lock(hashtext('if3110.migration'))`)
		})
		return nil
	}
	e.drain(w)
	j := e.job(id)
	if j.Status != "failed" || j.Hold != "commit_unknown" {
		t.Fatalf("job harus failed dengan hold commit_unknown: %+v", j)
	}
	pub := jobs.Public(j)
	if pub.Status != "commit_unknown" || !pub.CanResume || pub.CanCompensate {
		t.Errorf("API: %+v", pub)
	}
	eq(t, "attempts", e.flow.statuses(), "[commit_unknown commit_unknown]")

	// target kembali bisa dikunci: resume membaca bukti dulu, lalu melanjutkan tanpa double commit.
	holder.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('if3110.migration'))`)
	holder.Release()
	w.Hooks.BeforeCommit = nil
	w.LockWait = 2 * time.Second
	resume := e.enqueue(connA, batchPlan(upSQL), kind(jobs.KindResume, id))
	e.flow.plan = e.flow.attempts[0].Plan
	e.drain(w)
	if r := e.job(resume); r.Status != "success" {
		t.Fatalf("resume: %+v", r)
	}
	eq(t, "balance", balances(t, target), "map[101:1000]")
}

func TestCompensationJobRevertsInReverseOrderAndResumesAfterPartialFailure(t *testing.T) {
	e := newEnv(t, "w_comp")
	apply := e.enqueue(connA, batchPlan(upSQL))
	w := e.worker("w1")
	e.drain(w)
	eq(t, "setelah apply", balances(t, e.targets[connA]), "map[101:1000]")

	down := batchPlan(downSQL)
	down.Action = "rollback"
	comp := e.enqueue(connA, down, kind(jobs.KindCompensate, apply), retries(1))
	var order []int
	w.Hooks.BeforeCommit = func(op string, n int) error {
		if op == batch.OpDown {
			order = append(order, n)
			if n == 7 {
				return errors.New("gagal sebagian")
			}
		}
		return nil
	}
	e.drain(w)
	j := e.job(comp)
	if j.Status != "failed" || j.Hold != "partially_reverted" || j.RootID != apply {
		t.Fatalf("kompensasi gagal sebagian harus partially_reverted pada root yang sama: %+v", j)
	}
	if pub := jobs.Public(j); pub.Status != "partially_reverted" || !pub.CanCompensate || pub.CanResume {
		t.Errorf("API: %+v", pub)
	}
	if len(order) < 4 || fmt.Sprint(order[:4]) != "[10 9 8 7]" {
		t.Errorf("urutan batch harus terbalik: %v", order)
	}
	eq(t, "attempt terakhir", e.flow.statuses()[len(e.flow.statuses())-1], "partially_reverted")
	if b := balances(t, e.targets[connA]); b[100] != 300 || b[101] != 700 {
		t.Fatalf("batch 8-10 sudah dikembalikan, sisanya belum: %v", b)
	}

	// lanjutkan kompensasi: tidak mengulang batch yang sudah dikembalikan.
	order = nil
	w.Hooks.BeforeCommit = func(op string, n int) error { order = append(order, n); return nil }
	again := e.enqueue(connA, down, kind(jobs.KindCompensate, comp))
	e.drain(w)
	if r := e.job(again); r.Status != "success" {
		t.Fatalf("lanjutan: %+v", r)
	}
	eq(t, "urutan lanjutan", order, "[7 6 5 4 3 2 1]")
	eq(t, "balance akhir", balances(t, e.targets[connA]), "map[100:1000]")
}

func TestTraceContinuesFromEnqueueThroughWorkerToLockAndBatch(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	e := newEnv(t, "w_trace")

	// request API: span http.request, lalu trace context disimpan di job saat enqueue.
	apiCtx, apiSpan := otel.Tracer("api").Start(context.Background(), "http.request")
	stored := jobs.InjectTrace(apiCtx)
	apiSpan.End()
	if stored == "" {
		t.Fatal("trace context harus ikut tersimpan di job")
	}
	id := e.enqueue(connA, batchPlan(upSQL), func(n *jobs.NewJob) { n.TraceCtx = stored })
	e.drain(e.worker("w1"))
	eq(t, "status", e.job(id).Status, "success")

	want := apiSpan.SpanContext().TraceID()
	seen := map[string]bool{}
	for _, sp := range sr.Ended() {
		if sp.Name() == "http.request" {
			continue
		}
		seen[sp.Name()] = true
		if sp.SpanContext().TraceID() != want {
			t.Errorf("span %s harus satu trace dengan request API (%s), dapat %s", sp.Name(), want, sp.SpanContext().TraceID())
		}
		if sp.Name() == "job.run" && sp.Parent().SpanID() != apiSpan.SpanContext().SpanID() {
			t.Errorf("job.run harus anak dari span request API")
		}
	}
	for _, name := range []string{"job.run", "lock.acquire", "batch.run"} {
		if !seen[name] {
			t.Errorf("span %s tidak ditemukan", name)
		}
	}
}

type mariaTargets struct{}

func (mariaTargets) Target(context.Context, uuid.UUID) (*dbconn.Target, error) {
	return &dbconn.Target{DBMS: dbconn.MariaDB}, nil
}

func TestUnsupportedTargetFailsClearly(t *testing.T) {
	e := newEnv(t, "w_maria")
	id := e.enqueue(connA, normalPlan(`UPDATE perf.accounts SET balance = 1`))
	w := e.worker("w1")
	w.Targets = mariaTargets{}
	e.drain(w)
	j := e.job(id)
	if j.Status != "failed" || j.Hold != "none" || !strings.Contains(j.Error, "PostgreSQL") {
		t.Errorf("target non-PostgreSQL harus ditolak jelas: %+v", j)
	}
}
