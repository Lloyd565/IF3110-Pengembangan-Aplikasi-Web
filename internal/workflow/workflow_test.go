package workflow

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

var (
	conn  = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	alice = reqctx.Principal{UserID: 1, Username: "alice"} // edit, leader group app
	bob   = reqctx.Principal{UserID: 2, Username: "bob"}   // view only, leader group ro
	carol = reqctx.Principal{UserID: 3, Username: "carol"} // edit, anggota app
	dave  = reqctx.Principal{UserID: 4, Username: "dave"}  // tanpa akses
)

type fakeQueue struct{ plans []Plan }

func (q *fakeQueue) Enqueue(_ context.Context, _ pgx.Tx, p Plan, key string) (contracts.Job, error) {
	for _, x := range q.plans {
		if x.RequestID == p.RequestID {
			return contracts.Job{ID: "job-" + p.RequestID, RequestID: p.RequestID, Status: "pending"}, nil
		}
	}
	q.plans = append(q.plans, p)
	return contracts.Job{ID: "job-" + p.RequestID, RequestID: p.RequestID, Status: "pending"}, nil
}

func setup(t *testing.T, name string) (*Service, *fakeQueue, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Fresh(t, name)
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES (1,'alice','x'),(2,'bob','x'),(3,'carol','x'),(4,'dave','x');
		INSERT INTO groups (id, name) VALUES (10,'app'),(20,'ro');
		INSERT INTO group_members VALUES (10,1),(10,3),(20,2);
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc)
			VALUES ('aaaaaaaa-0000-0000-0000-000000000001','A','h',5432,'d','u','\x00');
		INSERT INTO db_grants VALUES (10,'aaaaaaaa-0000-0000-0000-000000000001','edit'),(20,'aaaaaaaa-0000-0000-0000-000000000001','view');
		INSERT INTO group_leaders (group_id, user_id) VALUES (10,1),(20,2);`); err != nil {
		t.Fatal(err)
	}
	q := &fakeQueue{}
	return &Service{DB: db, Authz: &authz.Authorizer{DB: db}, Queue: q}, q, db
}

func status(t *testing.T, err error) int {
	t.Helper()
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Status
	}
	t.Fatalf("err = %v, mau contracts.Error", err)
	return 0
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// approveAll membuat, mengirim, dan menyetujui pengajuan oleh semua leader.
func approveAll(t *testing.T, s *Service, in contracts.ChangeInput, leaders ...reqctx.Principal) contracts.ChangeRequest {
	t.Helper()
	ctx := context.Background()
	x := must[contracts.ChangeRequest](t)(s.Propose(ctx, carol, conn, in))
	must[contracts.ChangeRequest](t)(s.Submit(ctx, carol, conn, x.ID))
	for _, l := range leaders {
		x = must[contracts.ChangeRequest](t)(s.Review(ctx, l, conn, x.ID, contracts.Review{Decision: "approve"}))
	}
	return x
}

// run mensimulasikan worker: Prepare, lalu satu attempt per status hasil.
func run(t *testing.T, s *Service, rid string, results ...string) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	for i, result := range results {
		p := must[Plan](t)(s.Prepare(ctx, rid))
		id := must[string](t)(s.BeginAttempt(ctx, p, "job-"+rid, i+1))
		if err := s.FinishAttempt(ctx, id, result, ""); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestWorkflowIntegration(t *testing.T) {
	s, q, db := setup(t, "workflow")
	ctx := context.Background()

	m := must[contracts.Migration](t)(s.Save(ctx, carol, conn, "", contracts.Draft{Name: "t", SQLUp: "CREATE TABLE t (id int)", SQLDown: "DROP TABLE t"}))
	old := m.RevisionID
	m = must[contracts.Migration](t)(s.Save(ctx, carol, conn, m.ID, contracts.Draft{Name: "t", SQLUp: "CREATE TABLE t (id bigint)", SQLDown: "DROP TABLE t"}))
	if m.RevisionID == old || !m.CanEdit || m.Status != "draft" {
		t.Fatalf("revisi baru = %+v", m)
	}

	t.Run("revisi lama tidak bisa diajukan", func(t *testing.T) {
		x := must[contracts.ChangeRequest](t)(s.Propose(ctx, carol, conn, contracts.ChangeInput{MigrationID: m.ID, RevisionID: old, Action: "apply"}))
		if _, err := s.Submit(ctx, carol, conn, x.ID); err == nil {
			t.Fatal("submit revisi lama harus ditolak")
		}
	})

	x := must[contracts.ChangeRequest](t)(s.Propose(ctx, carol, conn, contracts.ChangeInput{MigrationID: m.ID, RevisionID: m.RevisionID, Action: "apply", Mode: "normal"}))
	x = must[contracts.ChangeRequest](t)(s.Submit(ctx, carol, conn, x.ID))
	if x.Status != "pending_review" || len(x.Approvals) != 2 {
		t.Fatalf("setelah submit = %+v", x)
	}

	t.Run("bukan leader tidak bisa review", func(t *testing.T) {
		_, err := s.Review(ctx, carol, conn, x.ID, contracts.Review{Decision: "approve"})
		if status(t, err) != 403 {
			t.Fatal(err)
		}
	})

	x = must[contracts.ChangeRequest](t)(s.Review(ctx, alice, conn, x.ID, contracts.Review{Decision: "approve"}))
	if x.Status != "pending_review" {
		t.Fatalf("satu dari dua leader belum cukup: %s", x.Status)
	}
	if _, err := s.Enqueue(ctx, alice, conn, x.ID, "key-12345"); status(t, err) != 409 {
		t.Fatal("enqueue sebelum seluruh approval harus 409")
	}
	// Leader View Only boleh approve.
	x = must[contracts.ChangeRequest](t)(s.Review(ctx, bob, conn, x.ID, contracts.Review{Decision: "approve"}))
	if x.Status != "approved" {
		t.Fatalf("setelah semua leader = %s", x.Status)
	}

	t.Run("pergantian leader membatalkan approval lama", func(t *testing.T) {
		if err := (&Leaders{DB: db}).Set(ctx, alice, 10, 3); err != nil {
			t.Fatal(err)
		}
		got := must[contracts.ChangeRequest](t)(s.Request(ctx, carol, conn, x.ID))
		if got.Status != "pending_review" || !got.CanApprove {
			t.Fatalf("setelah ganti leader = %s can_approve=%v", got.Status, got.CanApprove)
		}
		x = must[contracts.ChangeRequest](t)(s.Review(ctx, carol, conn, x.ID, contracts.Review{Decision: "approve"}))
		if x.Status != "approved" {
			t.Fatal(x.Status)
		}
	})

	t.Run("leader harus anggota group", func(t *testing.T) {
		if err := (&Leaders{DB: db}).Set(ctx, alice, 20, 4); status(t, err) != 422 {
			t.Fatal(err)
		}
	})

	job := must[contracts.Job](t)(s.Enqueue(ctx, carol, conn, x.ID, "key-12345"))
	again := must[contracts.Job](t)(s.Enqueue(ctx, carol, conn, x.ID, "key-12345"))
	if job.ID != again.ID || len(q.plans) != 1 || q.plans[0].SQL != "CREATE TABLE t (id bigint)" {
		t.Fatalf("enqueue idempotent: %+v %+v plans=%d", job, again, len(q.plans))
	}

	// Attempt 1 gagal, retry sukses: keduanya tercatat.
	ids := run(t, s, x.ID, "failed", "success")

	t.Run("definisi yang pernah dieksekusi immutable", func(t *testing.T) {
		_, err := s.Save(ctx, carol, conn, m.ID, contracts.Draft{Name: "t", SQLUp: "SELECT 1", SQLDown: "SELECT 1"})
		if status(t, err) != 409 {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE migration_executions SET sql = 'x' WHERE id = $1`, ids[0]); err == nil {
			t.Fatal("history harus append-only")
		}
		if _, err := db.Exec(ctx, `DELETE FROM migration_executions WHERE id = $1`, ids[1]); err == nil {
			t.Fatal("history tidak boleh dihapus")
		}
		if err := s.FinishAttempt(ctx, ids[0], "success", ""); status(t, err) != 409 {
			t.Fatal("attempt gagal tidak boleh diganti sukses")
		}
	})

	// Rollback memakai down asli dan merujuk execution yang dibatalkan.
	rb := approveAll(t, s, contracts.ChangeInput{MigrationID: m.ID, RevisionID: m.RevisionID, Action: "rollback", TargetExecutionID: ids[1]}, carol, bob)
	if rb.SQL != "DROP TABLE t" || rb.Status != "approved" {
		t.Fatalf("rollback = %+v", rb)
	}
	must[contracts.Job](t)(s.Enqueue(ctx, carol, conn, rb.ID, "key-rollback"))
	rbIDs := run(t, s, rb.ID, "success")

	full := must[[]contracts.Execution](t)(s.History(ctx, carol, conn, false))
	hidden := must[[]contracts.Execution](t)(s.History(ctx, carol, conn, true))
	if len(full) != 3 || full[2].RevertsExecutionID != ids[1] || !full[2].CanForward {
		t.Fatalf("history lengkap = %+v", full)
	}
	if len(hidden) != 1 || hidden[0].ID != ids[0] || hidden[0].Status != "failed" {
		t.Fatalf("hide rollback harus menyisakan attempt gagal saja: %+v", hidden)
	}

	// Forward menjalankan up revisi yang sama; tidak bisa mengganti SQL.
	fw := approveAll(t, s, contracts.ChangeInput{MigrationID: m.ID, RevisionID: m.RevisionID, Action: "forward", TargetExecutionID: rbIDs[0]}, carol, bob)
	if fw.SQL != "CREATE TABLE t (id bigint)" {
		t.Fatalf("forward SQL = %q", fw.SQL)
	}
	if got := must[contracts.Migration](t)(s.Get(ctx, carol, conn, m.ID)); got.Status != "rolled_back" || got.CanEdit {
		t.Fatalf("status definisi = %+v", got)
	}

	t.Run("irreversible tidak bisa di-rollback", func(t *testing.T) {
		ir := must[contracts.Migration](t)(s.Save(ctx, carol, conn, "", contracts.Draft{Name: "drop", SQLUp: "DROP TABLE IF EXISTS x", Irreversible: true, IrreversibleReason: "data hilang"}))
		ap := approveAll(t, s, contracts.ChangeInput{MigrationID: ir.ID, RevisionID: ir.RevisionID, Action: "apply"}, carol, bob)
		must[contracts.Job](t)(s.Enqueue(ctx, carol, conn, ap.ID, "key-irrev"))
		e := run(t, s, ap.ID, "success")
		h := must[[]contracts.Execution](t)(s.History(ctx, carol, conn, false))
		if h[len(h)-1].CanRollback {
			t.Fatal("irreversible tidak boleh punya can_rollback")
		}
		x := must[contracts.ChangeRequest](t)(s.Propose(ctx, carol, conn, contracts.ChangeInput{MigrationID: ir.ID, RevisionID: ir.RevisionID, Action: "rollback", TargetExecutionID: e[0]}))
		if !slices.Contains(x.Validation, "Migration irreversible tidak dapat di-rollback.") || x.CanSubmit {
			t.Fatalf("validasi = %v", x.Validation)
		}
	})

	t.Run("worker menolak jika izin actor dicabut", func(t *testing.T) {
		d := must[contracts.Migration](t)(s.Save(ctx, carol, conn, "", contracts.Draft{Name: "idx", SQLUp: "SELECT 1", SQLDown: "SELECT 1"}))
		ap := approveAll(t, s, contracts.ChangeInput{MigrationID: d.ID, RevisionID: d.RevisionID, Action: "apply"}, carol, bob)
		must[contracts.Job](t)(s.Enqueue(ctx, alice, conn, ap.ID, "key-revoke"))
		if _, err := db.Exec(ctx, `DELETE FROM group_members WHERE group_id = 10 AND user_id = 1`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prepare(ctx, ap.ID); status(t, err) != 403 {
			t.Fatal(err)
		}
	})
}

func TestUnresolvedAttemptBlocksOtherRequests(t *testing.T) {
	s, _, _ := setup(t, "workflow_block")
	ctx := context.Background()
	a := must[contracts.Migration](t)(s.Save(ctx, carol, conn, "", contracts.Draft{Name: "a", SQLUp: "SELECT 1", SQLDown: "SELECT 1"}))
	b := must[contracts.Migration](t)(s.Save(ctx, carol, conn, "", contracts.Draft{Name: "b", SQLUp: "SELECT 2", SQLDown: "SELECT 2"}))
	ap := approveAll(t, s, contracts.ChangeInput{MigrationID: a.ID, RevisionID: a.RevisionID, Action: "apply"}, alice, bob)
	must[contracts.Job](t)(s.Enqueue(ctx, carol, conn, ap.ID, "key-a"))
	run(t, s, ap.ID, "commit_unknown")

	x := must[contracts.ChangeRequest](t)(s.Propose(ctx, carol, conn, contracts.ChangeInput{MigrationID: b.ID, RevisionID: b.RevisionID, Action: "apply"}))
	if len(x.Validation) == 0 || x.CanSubmit {
		t.Fatalf("commit_unknown harus menahan migration lain: %+v", x.Validation)
	}
}

func TestLegacyBackfill(t *testing.T) {
	db := testdb.Fresh(t, "workflow_legacy")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Kembalikan ke schema M1, isi data lama, lalu jalankan upgrade lagi.
	if _, err := db.Exec(ctx, `
		DROP TABLE migration_approvals, migration_requests, migration_executions, migration_revisions, group_leaders CASCADE;
		DROP FUNCTION migration_revisions_immutable, migration_executions_guard;
		DELETE FROM schema_migrations WHERE version = '0003_workflow';
		INSERT INTO users (id, username, password_hash) VALUES (1,'budi','x');
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc)
			VALUES ('aaaaaaaa-0000-0000-0000-000000000001','A','h',5432,'d','u','\x00');
		INSERT INTO migrations (id, connection_id, version, name, sql, status, created_by, executed_by) VALUES
			(1,'aaaaaaaa-0000-0000-0000-000000000001',1,'v1','CREATE TABLE a (id int)','success',1,1),
			(2,'aaaaaaaa-0000-0000-0000-000000000001',2,'v2','bad sql','failed',1,1),
			(3,'aaaaaaaa-0000-0000-0000-000000000001',NULL,'draft','SELECT 1','draft',1,NULL);`); err != nil {
		t.Fatal(err)
	}
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: db, Authz: &authz.Authorizer{DB: db}}
	p := reqctx.Principal{UserID: 1}
	list := must[[]contracts.Migration](t)(s.List(ctx, p, conn))
	if len(list) != 3 {
		t.Fatalf("migration lama hilang: %d", len(list))
	}
	for _, m := range list {
		if !m.Irreversible || m.IrreversibleReason == "" {
			t.Errorf("legacy %s harus irreversible berlabel: %+v", m.Name, m)
		}
		if (m.Name == "draft") != m.CanEdit {
			t.Errorf("%s can_edit = %v", m.Name, m.CanEdit)
		}
	}
	h := must[[]contracts.Execution](t)(s.History(ctx, p, conn, false))
	if len(h) != 2 || h[0].MigrationName != "v1" || h[0].Status != "success" || h[1].Status != "failed" || h[0].SQL != "CREATE TABLE a (id int)" {
		t.Fatalf("history legacy = %+v", h)
	}
	// Digest backfill memakai rumus yang sama dengan Go.
	var dbDigest string
	if err := db.QueryRow(ctx, `SELECT digest FROM migration_revisions WHERE migration_id = 1`).Scan(&dbDigest); err != nil {
		t.Fatal(err)
	}
	if want := digest(contracts.Draft{SQLUp: "CREATE TABLE a (id int)", Irreversible: true, IrreversibleReason: "Legacy M1: SQL down tidak tersedia."}); dbDigest != want {
		t.Fatalf("digest backfill %s != %s", dbDigest, want)
	}
}
