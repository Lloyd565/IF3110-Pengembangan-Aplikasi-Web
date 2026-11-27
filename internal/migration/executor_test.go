package migration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func TestCheckTransactional(t *testing.T) {
	allowed := []string{
		"ALTER TABLE shop.customers ADD COLUMN phone text;",
		"CREATE TABLE a (id int); INSERT INTO a VALUES (1); -- vacuum nanti",
		"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END $$ LANGUAGE plpgsql;",
		"CREATE FUNCTION g() RETURNS int AS $body$ BEGIN COMMIT; END $body$ LANGUAGE plpgsql;",
		"INSERT INTO log (msg) VALUES ('commit; vacuum; begin');",
		"/* drop database x; */ CREATE INDEX idx ON a (id);",
		"REFRESH MATERIALIZED VIEW CONCURRENTLY mv;",
		`CREATE TABLE "begin" (id int);`,
	}
	for _, sql := range allowed {
		if err := CheckTransactional(sql); err != nil {
			t.Errorf("harus diizinkan: %q\n  err: %v", sql, err)
		}
	}

	blocked := []string{
		"BEGIN; CREATE TABLE a (id int); COMMIT;",
		"CREATE TABLE a (id int); commit;",
		"start transaction",
		"ROLLBACK",
		"VACUUM shop.customers;",
		"vacuum full",
		"CREATE INDEX CONCURRENTLY idx ON a (id);",
		"create   unique index\n concurrently idx ON a (id)",
		"DROP INDEX CONCURRENTLY idx;",
		"CREATE DATABASE x;",
		"ALTER SYSTEM SET work_mem = '64MB';",
		"-- cuma komentar",
		" ; ; ",
	}
	for _, sql := range blocked {
		if err := CheckTransactional(sql); !errors.As(err, new(ValidationError)) {
			t.Errorf("harus ditolak: %q (err = %v)", sql, err)
		}
	}
}

type execEnv struct {
	platform, target *pgxpool.Pool
}

func newExecEnv(t *testing.T) execEnv {
	t.Helper()
	ctx := context.Background()
	platform := testdb.Fresh(t, "migration_exec")
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Exec(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES (1, 'budi', 'x');
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc) VALUES
			('aaaaaaaa-0000-0000-0000-000000000001', 'A', 'h', 5432, 'd', 'u', '\x00'),
			('bbbbbbbb-0000-0000-0000-000000000002', 'B', 'h', 5432, 'd', 'u', '\x00');`); err != nil {
		t.Fatal(err)
	}
	return execEnv{platform: platform, target: testdb.Fresh(t, "migration_target")}
}

func (e execEnv) draft(t *testing.T, sql string) int64 {
	t.Helper()
	var id int64
	if err := e.platform.QueryRow(context.Background(),
		`INSERT INTO migrations (connection_id, name, sql, created_by) VALUES ($1, 'm', $2, 1) RETURNING id`,
		connA, sql).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e execEnv) forceRunning(t *testing.T, id int64) {
	t.Helper()
	if _, err := e.platform.Exec(context.Background(), `
		UPDATE migrations SET status = 'running',
			version = (SELECT coalesce(max(version), 0) + 1 FROM migrations WHERE connection_id = $2)
		WHERE id = $1`, id, connA); err != nil {
		t.Fatal(err)
	}
}

func (e execEnv) get(t *testing.T, id int64) *Migration {
	t.Helper()
	m, err := Get(context.Background(), e.platform, connA, id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e execEnv) tableExists(t *testing.T, name string) bool {
	t.Helper()
	var ok bool
	if err := e.target.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestExecute(t *testing.T) {
	e := newExecEnv(t)
	ctx := context.Background()

	t.Run("sukses: multi-statement di-commit, versi 1", func(t *testing.T) {
		id := e.draft(t, "CREATE TABLE items (id int PRIMARY KEY, name text); INSERT INTO items VALUES (1, 'a'), (2, 'b');")
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); err != nil {
			t.Fatal(err)
		}
		m := e.get(t, id)
		if m.Status != StatusSuccess || m.Version == nil || *m.Version != 1 || m.ErrorMessage != nil {
			t.Errorf("hasil = status %s, version %v, err %v", m.Status, m.Version, m.ErrorMessage)
		}
		if m.ExecutedBy == nil || *m.ExecutedBy != 1 || m.StartedAt == nil || m.FinishedAt == nil {
			t.Errorf("pelaksana/waktu tidak tercatat: %+v", m)
		}
		var n int
		e.target.QueryRow(ctx, `SELECT count(*) FROM items`).Scan(&n)
		if n != 2 {
			t.Errorf("isi items = %d baris, mau 2", n)
		}
	})

	t.Run("gagal: SQL error di-rollback seluruhnya, versi 2", func(t *testing.T) {
		id := e.draft(t, "CREATE TABLE half_done (id int); SELECT 1/0;")
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); err != nil {
			t.Fatal(err)
		}
		m := e.get(t, id)
		if m.Status != StatusFailed || *m.Version != 2 || m.ErrorMessage == nil || !strings.Contains(*m.ErrorMessage, "division by zero") {
			t.Errorf("hasil = status %s, version %v, err %v", m.Status, m.Version, m.ErrorMessage)
		}
		if e.tableExists(t, "half_done") {
			t.Error("tabel half_done harusnya ikut di-rollback")
		}
	})

	t.Run("migration yang sudah dieksekusi tidak bisa dijalankan ulang", func(t *testing.T) {
		id := e.draft(t, "SELECT 1")
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); err != nil {
			t.Fatal(err)
		}
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); !errors.Is(err, ErrNotDraft) {
			t.Errorf("eksekusi ulang: err = %v, mau ErrNotDraft", err)
		}
	})

	t.Run("migration koneksi lain tidak bisa dieksekusi", func(t *testing.T) {
		id := e.draft(t, "SELECT 1")
		if err := Execute(ctx, e.platform, e.target, connB, id, 1); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, mau ErrNotFound", err)
		}
		if m := e.get(t, id); m.Status != StatusDraft {
			t.Errorf("status berubah jadi %s", m.Status)
		}
	})

	t.Run("perintah non-transaksi ditolak, tetap draft", func(t *testing.T) {
		id := e.draft(t, "VACUUM items;")
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); !errors.As(err, new(ValidationError)) {
			t.Errorf("err = %v, mau ValidationError", err)
		}
		if m := e.get(t, id); m.Status != StatusDraft || m.Version != nil {
			t.Errorf("harus tetap draft tanpa versi: %s %v", m.Status, m.Version)
		}
	})

	t.Run("ditolak kalau ada migration lain yang running", func(t *testing.T) {
		running := e.draft(t, "SELECT 1")
		e.forceRunning(t, running)
		defer e.platform.Exec(ctx, `UPDATE migrations SET status = 'failed' WHERE id = $1`, running)

		id := e.draft(t, "SELECT 1")
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); !errors.Is(err, ErrBusy) {
			t.Errorf("err = %v, mau ErrBusy", err)
		}
	})

	t.Run("target mati: tetap draft tanpa versi", func(t *testing.T) {
		dead, err := pgxpool.New(ctx, "postgres://x:x@127.0.0.1:1/x?connect_timeout=1")
		if err != nil {
			t.Fatal(err)
		}
		defer dead.Close()
		id := e.draft(t, "SELECT 1")
		if err := Execute(ctx, e.platform, dead, connA, id, 1); !errors.Is(err, ErrTargetUnavailable) {
			t.Errorf("err = %v, mau ErrTargetUnavailable", err)
		}
		if m := e.get(t, id); m.Status != StatusDraft || m.Version != nil {
			t.Errorf("harus tetap draft tanpa versi: %s %v", m.Status, m.Version)
		}
	})

	t.Run("statement_timeout menghentikan SQL yang terlalu lama", func(t *testing.T) {
		old := StatementTimeout
		StatementTimeout = 300 * time.Millisecond
		defer func() { StatementTimeout = old }()

		id := e.draft(t, "SELECT pg_sleep(3);")
		start := time.Now()
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); err != nil {
			t.Fatal(err)
		}
		m := e.get(t, id)
		if m.Status != StatusFailed || !strings.Contains(*m.ErrorMessage, "statement timeout") {
			t.Errorf("hasil = %s, %v", m.Status, m.ErrorMessage)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("timeout tidak bekerja, butuh %v", time.Since(start))
		}
	})

	t.Run("lock_timeout: tabel yang sedang dikunci tidak ditunggu selamanya", func(t *testing.T) {
		old := LockTimeout
		LockTimeout = 300 * time.Millisecond
		defer func() { LockTimeout = old }()

		holder, err := e.target.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		if _, err := holder.Exec(ctx, `LOCK TABLE items IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}

		id := e.draft(t, "ALTER TABLE items ADD COLUMN extra text;")
		if err := Execute(ctx, e.platform, e.target, connA, id, 1); err != nil {
			t.Fatal(err)
		}
		m := e.get(t, id)
		if m.Status != StatusFailed || !strings.Contains(*m.ErrorMessage, "lock timeout") {
			t.Errorf("hasil = %s, %v", m.Status, m.ErrorMessage)
		}
	})

	t.Run("request dibatalkan: hasil tetap tercatat", func(t *testing.T) {
		id := e.draft(t, "SELECT pg_sleep(3);")
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if err := Execute(cctx, e.platform, e.target, connA, id, 1); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t, id); m.Status != StatusFailed || m.FinishedAt == nil {
			t.Errorf("status = %s, finished = %v; mau failed dan tercatat", m.Status, m.FinishedAt)
		}
	})

	t.Run("dua eksekusi bersamaan: SQL hanya jalan sekali", func(t *testing.T) {
		if _, err := e.target.Exec(ctx, `CREATE TABLE counter (n int); INSERT INTO counter VALUES (0)`); err != nil {
			t.Fatal(err)
		}
		id := e.draft(t, "UPDATE counter SET n = n + 1; SELECT pg_sleep(0.2);")
		errs := make(chan error, 2)
		for range 2 {
			go func() { errs <- Execute(ctx, e.platform, e.target, connA, id, 1) }()
		}
		var ok, rejected int
		for range 2 {
			switch err := <-errs; {
			case err == nil:
				ok++
			case errors.Is(err, ErrNotDraft), errors.Is(err, ErrBusy):
				rejected++
			default:
				t.Errorf("err tak terduga: %v", err)
			}
		}
		var n int
		e.target.QueryRow(ctx, `SELECT n FROM counter`).Scan(&n)
		if ok != 1 || rejected != 1 || n != 1 {
			t.Errorf("ok=%d ditolak=%d counter=%d; mau 1, 1, 1", ok, rejected, n)
		}
	})

	t.Run("versi selalu berurutan", func(t *testing.T) {
		list, err := List(ctx, e.platform, connA)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[int]bool{}
		for _, m := range list {
			if m.Version != nil {
				seen[*m.Version] = true
			}
		}
		for v := 1; v <= len(seen); v++ {
			if !seen[v] {
				t.Errorf("versi %d hilang, ada: %v", v, seen)
			}
		}
	})

	t.Run("riwayat di target ikut transaksi migration", func(t *testing.T) {
		ok := e.draft(t, "CREATE TABLE hist_ok (id int);")
		bad := e.draft(t, "CREATE TABLE hist_bad (id int); SELECT 1/0;")
		for _, id := range []int64{ok, bad} {
			if err := Execute(ctx, e.platform, e.target, connA, id, 1); err != nil {
				t.Fatal(err)
			}
		}
		if !e.inHistory(t, ok) || e.inHistory(t, bad) {
			t.Errorf("riwayat: sukses=%v gagal=%v; mau true, false", e.inHistory(t, ok), e.inHistory(t, bad))
		}
	})

	t.Run("hasil ambigu (koneksi putus saat commit): target yang menentukan", func(t *testing.T) {
		applied := e.draft(t, "CREATE TABLE ambiguous_ok (id int);")
		r := pgRunner(e.target)
		run := r.run
		r.run = func(ctx context.Context, id int64, sql string) error {
			if err := run(ctx, id, sql); err != nil {
				return err
			}
			return errors.New("koneksi putus")
		}
		if err := execute(ctx, e.platform, connA, applied, 1, r); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t, applied); m.Status != StatusSuccess {
			t.Errorf("status = %s, mau success (commit sebenarnya berhasil)", m.Status)
		}

		notApplied := e.draft(t, "CREATE TABLE ambiguous_no (id int);")
		r.run = func(context.Context, int64, string) error { return errors.New("koneksi putus") }
		if err := execute(ctx, e.platform, connA, notApplied, 1, r); err != nil {
			t.Fatal(err)
		}
		if m := e.get(t, notApplied); m.Status != StatusFailed {
			t.Errorf("status = %s, mau failed", m.Status)
		}
	})

	t.Run("platform gagal menyimpan status: disinkronkan ulang di background", func(t *testing.T) {
		id := e.draft(t, "CREATE TABLE pending_ok (id int);")
		r := pgRunner(e.target)
		run := r.run
		r.run = func(ctx context.Context, id int64, sql string) error {
			err := run(ctx, id, sql)

			e.platform.Exec(ctx, `ALTER TABLE migrations ADD CONSTRAINT block_settle CHECK (status <> 'success') NOT VALID`)
			return err
		}
		if err := execute(ctx, e.platform, connA, id, 1, r); !errors.Is(err, ErrStatusPending) {
			t.Fatalf("err = %v, mau ErrStatusPending", err)
		}
		if m := e.get(t, id); m.Status != StatusRunning {
			t.Fatalf("status = %s, mau tetap running", m.Status)
		}
		if _, err := e.platform.Exec(ctx, `ALTER TABLE migrations DROP CONSTRAINT block_settle`); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for e.get(t, id).Status == StatusRunning && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if m := e.get(t, id); m.Status != StatusSuccess {
			t.Errorf("status = %s, mau success setelah retry", m.Status)
		}
	})

	t.Run("pemulihan saat start: status mengikuti riwayat di target", func(t *testing.T) {
		applied := e.draft(t, "SELECT 1")
		e.forceRunning(t, applied)
		if _, err := e.target.Exec(ctx, `INSERT INTO `+pgHistory+` (migration_id) VALUES ($1)`, applied); err != nil {
			t.Fatal(err)
		}
		lost := e.draft(t, "SELECT 1")
		e.forceRunning(t, lost)

		open := func(context.Context, uuid.UUID) (*dbconn.Target, error) {
			return &dbconn.Target{DBMS: dbconn.PostgreSQL, Postgres: e.target}, nil
		}
		if n, err := RecoverInterrupted(ctx, e.platform, open); err != nil || n != 2 {
			t.Fatalf("n = %d, err = %v", n, err)
		}
		if m := e.get(t, applied); m.Status != StatusSuccess {
			t.Errorf("ada di riwayat: status = %s, mau success", m.Status)
		}
		if m := e.get(t, lost); m.Status != StatusFailed || !strings.Contains(*m.ErrorMessage, "tidak di-commit") {
			t.Errorf("tidak ada di riwayat: status = %s, %v", m.Status, m.ErrorMessage)
		}
	})
}

func (e execEnv) inHistory(t *testing.T, id int64) bool {
	t.Helper()
	var ok bool
	if err := e.target.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM `+pgHistory+` WHERE migration_id = $1)`, id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}
