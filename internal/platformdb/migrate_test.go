package platformdb

import (
	"context"
	"io/fs"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func TestMigrate(t *testing.T) {
	db := testdb.Fresh(t, "platformdb")
	ctx := context.Background()

	for i := range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("Migrate ke-%d: %v", i+1, err)
		}
	}

	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '0001_init'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("0001_init tercatat %d kali, err=%v", n, err)
	}

	mustFail := map[string]string{
		"username case-insensitive": `INSERT INTO users (username, password_hash) VALUES ('Budi','x'), ('budi','x')`,
		"draft tidak boleh punya version": `INSERT INTO users (id, username, password_hash) VALUES (99,'u99','x');
			INSERT INTO db_connections (id,name,host,port,dbname,username,password_enc) VALUES ('00000000-0000-0000-0000-000000000001','c','h',5432,'d','u','\x00');
			INSERT INTO migrations (connection_id,version,name,sql,created_by) VALUES ('00000000-0000-0000-0000-000000000001',1,'m','select 1',99)`,
		"level grant hanya view/edit": `INSERT INTO groups (id,name) VALUES (1,'g');
			INSERT INTO db_connections (id,name,host,port,dbname,username,password_enc) VALUES ('00000000-0000-0000-0000-000000000002','c2','h',5432,'d','u','\x00');
			INSERT INTO db_grants VALUES (1,'00000000-0000-0000-0000-000000000002','admin')`,
	}
	for name, sql := range mustFail {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql); err == nil {
			t.Errorf("%s: seharusnya ditolak DB", name)
		}
		tx.Rollback(ctx)
	}
}

func TestMigrationFilesEmbedded(t *testing.T) {
	names, _ := fs.Glob(migrationFiles, "migrations/*.sql")
	if len(names) == 0 {
		t.Fatal("tidak ada file migrations/*.sql yang ter-embed; cek baris //go:embed di migrate.go")
	}
}

func TestConnectionDBMSUpgrade(t *testing.T) {
	db := testdb.Fresh(t, "platformdb_dbms_upgrade")
	ctx := context.Background()
	initial, err := migrationFiles.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE schema_migrations(version text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migrations(version) VALUES('0001_init');
		INSERT INTO db_connections(id,name,host,port,dbname,username,password_enc) VALUES('00000000-0000-0000-0000-000000000001','legacy','h',5432,'d','u','\x0102')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var kind string
	var blob []byte
	if err := db.QueryRow(ctx, `SELECT dbms,password_enc FROM db_connections WHERE name='legacy'`).Scan(&kind, &blob); err != nil || kind != "postgresql" || len(blob) != 2 {
		t.Fatal("legacy connection changed", err)
	}
	if _, err := db.Exec(ctx, `UPDATE db_connections SET dbms='unknown' WHERE name='legacy'`); err == nil {
		t.Fatal("unknown DBMS accepted")
	}
}
