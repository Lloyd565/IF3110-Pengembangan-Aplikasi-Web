package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

var (
	connA = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	connB = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
)

func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Fresh(t, "jobs")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc) VALUES
		($1, 'A', 'h', 5432, 'd', 'u', '\x00'), ($2, 'B', 'h', 5432, 'd', 'u', '\x00')`, connA, connB); err != nil {
		t.Fatal(err)
	}
	return db
}

func enqueue(t *testing.T, db *pgxpool.Pool, conn uuid.UUID, key string) int64 {
	t.Helper()
	id, _, err := Enqueue(context.Background(), db, NewJob{
		ConnectionID: conn, RequestID: uuid.New(), Key: key, Plan: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnqueueIdempotent(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	in := NewJob{ConnectionID: connA, RequestID: uuid.New(), Key: "k1", Plan: json.RawMessage(`{}`)}
	first, created, err := Enqueue(ctx, db, in)
	if err != nil || !created {
		t.Fatalf("enqueue pertama: id=%d created=%v err=%v", first, created, err)
	}
	again, created, err := Enqueue(ctx, db, in)
	if err != nil || created || again != first {
		t.Fatalf("enqueue ulang harus mengembalikan job lama: id=%d created=%v err=%v", again, created, err)
	}
	var n int
	db.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n)
	if n != 1 {
		t.Errorf("jumlah job = %d, mau 1", n)
	}
}

func TestResumeSharesRootWithParent(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	root := enqueue(t, db, connA, "root")
	child, _, err := Enqueue(ctx, db, NewJob{ConnectionID: connA, RequestID: uuid.New(), Key: "child", Kind: KindResume,
		ParentJobID: &root, Plan: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := Get(ctx, db, child)
	if j.RootID != root || j.Kind != KindResume {
		t.Errorf("root=%d kind=%s, mau root=%d kind=resume", j.RootID, j.Kind, root)
	}
	if r, _ := Get(ctx, db, root); r.RootID != root {
		t.Errorf("job utama harus menjadi root dirinya sendiri, root=%d", r.RootID)
	}
}

func TestClaimSerialPerDatabase(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	a1, a2, b1 := enqueue(t, db, connA, "a1"), enqueue(t, db, connA, "a2"), enqueue(t, db, connB, "b1")

	j1, err := Claim(ctx, db, "w1", time.Minute)
	if err != nil || j1 == nil || j1.ID != a1 {
		t.Fatalf("klaim pertama: %+v err=%v", j1, err)
	}
	// a2 satu database dengan a1 yang sedang jalan: dilewati; b1 database lain boleh paralel.
	j2, err := Claim(ctx, db, "w2", time.Minute)
	if err != nil || j2 == nil || j2.ID != b1 {
		t.Fatalf("klaim kedua harus b1 (database berbeda), dapat %+v err=%v", j2, err)
	}
	if j3, err := Claim(ctx, db, "w3", time.Minute); err != nil || j3 != nil {
		t.Fatalf("a2 tidak boleh diklaim selama a1 berjalan: %+v err=%v", j3, err)
	}
	if err := Succeed(ctx, db, a1, "w1"); err != nil {
		t.Fatal(err)
	}
	if j3, err := Claim(ctx, db, "w3", time.Minute); err != nil || j3 == nil || j3.ID != a2 {
		t.Fatalf("setelah a1 selesai a2 harus bisa diklaim: %+v err=%v", j3, err)
	}
}

func TestClaimConcurrentNoDoubleClaim(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		enqueue(t, db, connA, fmt.Sprintf("k%d", i))
	}
	var mu sync.Mutex
	var got []int64
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			j, err := Claim(ctx, db, fmt.Sprintf("w%d", w), time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			if j != nil {
				mu.Lock()
				got = append(got, j.ID)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(got) != 1 {
		t.Fatalf("pada satu database hanya satu job boleh running, dapat %v", got)
	}
}

func TestLeaseExpiryFencesOldWorker(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	id := enqueue(t, db, connA, "k")
	if j, err := Claim(ctx, db, "lama", time.Minute); err != nil || j == nil {
		t.Fatalf("klaim: %v %v", j, err)
	}
	// worker lama mati: lease dianggap kedaluwarsa.
	if _, err := db.Exec(ctx, `UPDATE jobs SET lease_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	j, err := Claim(ctx, db, "baru", time.Minute)
	if err != nil || j == nil || j.ID != id || j.WorkerID != "baru" || j.RetryCount != 1 {
		t.Fatalf("worker baru harus mengambil alih: %+v err=%v", j, err)
	}
	for name, err := range map[string]error{
		"heartbeat": Heartbeat(ctx, db, id, "lama", time.Minute),
		"succeed":   Succeed(ctx, db, id, "lama"),
		"progress":  SetProgress(ctx, db, id, "lama", 1, 1, nil, HoldNone),
		"execution": SetExecution(ctx, db, id, "lama", uuid.NewString(), 2),
	} {
		if !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s oleh worker lama harus ErrLeaseLost, dapat %v", name, err)
		}
	}
	if err := Succeed(ctx, db, id, "baru"); err != nil {
		t.Errorf("worker baru harus bisa menyelesaikan: %v", err)
	}
}

func TestRetryBackoffAndExhaustion(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	id, _, err := Enqueue(ctx, db, NewJob{ConnectionID: connA, RequestID: uuid.New(), Key: "k",
		Plan: json.RawMessage(`{}`), MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := Claim(ctx, db, "w", time.Minute)
	again, err := Retry(ctx, db, j, "w", HoldNone, "koneksi putus", time.Hour)
	if err != nil || !again {
		t.Fatalf("retry pertama: again=%v err=%v", again, err)
	}
	if got, _ := Claim(ctx, db, "w", time.Minute); got != nil {
		t.Fatal("job retry_wait belum boleh diklaim sebelum next_retry_at")
	}
	db.Exec(ctx, `UPDATE jobs SET next_retry_at = now() - interval '1 second' WHERE id = $1`, id)
	j, err = Claim(ctx, db, "w", time.Minute)
	if err != nil || j == nil || j.RetryCount != 1 {
		t.Fatalf("klaim setelah backoff: %+v err=%v", j, err)
	}
	again, err = Retry(ctx, db, j, "w", HoldNone, "gagal lagi", time.Hour)
	if err != nil || again {
		t.Fatalf("jatah habis harus berhenti: again=%v err=%v", again, err)
	}
	got, _ := Get(ctx, db, id)
	if got.Status != StatusFailed || got.Error != "gagal lagi" {
		t.Errorf("status akhir = %s (%s), mau failed", got.Status, got.Error)
	}
}

func TestQueueEnqueueOncePerRequest(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	req := uuid.New()
	plan := workflow.Plan{RequestID: req.String(), ConnectionID: connA, MigrationID: "7", Action: "apply", Mode: "normal", SQL: "SELECT 1"}
	enq := func(p workflow.Plan, key string) (contracts.Job, error) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		j, err := Queue{}.Enqueue(ctx, tx, p, key)
		if err == nil {
			err = tx.Commit(ctx)
		}
		return j, err
	}
	first, err := enq(plan, "k1")
	if err != nil || first.Status != "queued" || first.MigrationID != "7" {
		t.Fatalf("enqueue pertama: %+v err=%v", first, err)
	}
	// kunci berbeda untuk pengajuan yang sama tidak boleh membuat job kedua.
	again, err := enq(plan, "k2")
	if err != nil || again.ID != first.ID {
		t.Fatalf("pengajuan sama harus mengembalikan job yang sama: %+v err=%v", again, err)
	}
	// kunci yang sama untuk pengajuan lain adalah konflik.
	other := plan
	other.RequestID = uuid.NewString()
	_, err = enq(other, "k1")
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Status != 409 {
		t.Fatalf("idempotency key bentrok harus 409, dapat %v", err)
	}
	var n int
	db.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n)
	if n != 1 {
		t.Errorf("jumlah job = %d, mau 1", n)
	}
}

func TestPublicStatusMapping(t *testing.T) {
	cases := []struct {
		status, hold, want string
		resume, compensate bool
	}{
		{StatusQueued, HoldNone, "queued", false, false},
		{StatusRetryWait, HoldNone, "queued", false, false},
		{StatusSucceeded, HoldNone, "success", false, false},
		{StatusFailed, HoldNone, "failed", false, false},
		{StatusFailed, HoldPartial, "partial", true, true},
		{StatusFailed, HoldPartiallyReverted, "partially_reverted", false, true},
		{StatusFailed, HoldCommitUnknown, "commit_unknown", true, false},
	}
	for _, c := range cases {
		out := Public(&Job{Status: c.status, Hold: c.hold, Plan: json.RawMessage(`{}`)})
		if out.Status != c.want || out.CanResume != c.resume || out.CanCompensate != c.compensate {
			t.Errorf("%s/%s: status=%s resume=%v compensate=%v, mau %s %v %v", c.status, c.hold,
				out.Status, out.CanResume, out.CanCompensate, c.want, c.resume, c.compensate)
		}
	}
}
