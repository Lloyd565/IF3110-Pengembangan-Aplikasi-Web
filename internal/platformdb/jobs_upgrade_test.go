package platformdb

import (
	"context"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

// Database yang sudah pernah menjalankan 0004 versi awal (yang sempat dipublikasikan) harus ikut
// ter-upgrade ke skema jobs yang baru lewat 0005, tanpa mengedit migration yang sudah terbit.
func TestJobsSchemaUpgradesFromPublishedVersion(t *testing.T) {
	db := testdb.Fresh(t, "jobs_upgrade")
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// kembalikan ke keadaan mesin lama: tabel jobs hasil 0004 asli, 0005 belum tercatat.
	old, err := migrationFiles.ReadFile("migrations/0004_jobs.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE jobs`, string(old), `DELETE FROM schema_migrations WHERE version = '0005_jobs_v2'`} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var legacy bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'operation')`).Scan(&legacy); err != nil || !legacy {
		t.Fatalf("prasyarat: tabel lama harus terpasang (legacy=%v err=%v)", legacy, err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("upgrade gagal: %v", err)
	}
	for _, col := range []string{"kind", "plan", "root_job_id", "attempt_open", "request_id"} {
		var ok bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = $1)`, col).Scan(&ok); err != nil || !ok {
			t.Errorf("kolom %s harus ada setelah upgrade", col)
		}
	}
	var gone bool
	db.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'operation')`).Scan(&gone)
	if !gone {
		t.Error("kolom skema lama harus hilang")
	}
	// migrasi ulang idempoten.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrasi kedua harus no-op: %v", err)
	}
}
