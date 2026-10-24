package dbconn

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

// targetParams: parameter koneksi ke database `dbname` di server test yang sama.
func targetParams(t *testing.T, dbname string) Params {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	return Params{Host: cfg.Host, Port: int(cfg.Port), DBName: dbname,
		Username: cfg.User, Password: cfg.Password, SSLMode: "disable"}
}

func insertConn(t *testing.T, db *pgxpool.Pool, reg *Registry, name string, p Params) uuid.UUID {
	t.Helper()
	id := uuid.New()
	enc, err := reg.Seal(id, p.Password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc, sslmode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, name, p.Host, p.Port, p.DBName, p.Username, enc, p.SSLMode); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRegistry(t *testing.T) {
	t.Setenv("DEV_TARGET_DATABASE_URL", "")
	ctx := context.Background()
	platform := testdb.Fresh(t, "dbconn")
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	testdb.Fresh(t, "dbconn_target")

	reg := &Registry{DB: platform, Key: randKey(t)}
	good := insertConn(t, platform, reg, "good", targetParams(t, "it_dbconn_target"))

	t.Run("pool tersambung dan dipakai ulang", func(t *testing.T) {
		p1, err := reg.Pool(ctx, good)
		if err != nil {
			t.Fatal(err)
		}
		var one int
		if err := p1.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
			t.Fatalf("query ke target: %d, %v", one, err)
		}
		p2, _ := reg.Pool(ctx, good)
		if p1 != p2 {
			t.Error("pool harus di-cache per koneksi")
		}
		reg.Invalidate(good)
		p3, err := reg.Pool(ctx, good)
		if err != nil || p3 == p1 {
			t.Errorf("setelah Invalidate harus dibuat pool baru: %v", err)
		}
	})

	t.Run("target mati menghasilkan error dan tidak di-cache", func(t *testing.T) {
		dead := targetParams(t, "x")
		dead.Port = 1
		id := insertConn(t, platform, reg, "dead", dead)
		if _, err := reg.Pool(ctx, id); err == nil {
			t.Fatal("koneksi ke port tertutup harus error")
		}
		if reg.pools[id] != nil {
			t.Error("pool gagal tidak boleh disimpan")
		}
	})

	t.Run("koneksi tidak dikenal atau sudah dihapus", func(t *testing.T) {
		if _, err := reg.Pool(ctx, uuid.New()); !errors.Is(err, ErrUnknownConnection) {
			t.Errorf("id tak dikenal: %v", err)
		}
		gone := insertConn(t, platform, reg, "gone", targetParams(t, "it_dbconn_target"))
		if _, err := platform.Exec(ctx, `UPDATE db_connections SET deleted_at = now() WHERE id = $1`, gone); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.Pool(ctx, gone); !errors.Is(err, ErrUnknownConnection) {
			t.Errorf("koneksi soft-delete: %v", err)
		}
	})

	t.Run("kunci salah atau blob dipindah ke koneksi lain gagal", func(t *testing.T) {
		other := &Registry{DB: platform, Key: randKey(t)}
		if _, err := other.Pool(ctx, good); err == nil {
			t.Error("kunci yang berbeda harus gagal mendekripsi")
		}

		a := insertConn(t, platform, reg, "a", targetParams(t, "it_dbconn_target"))
		b := insertConn(t, platform, reg, "b", targetParams(t, "it_dbconn_target"))
		if _, err := platform.Exec(ctx, `
			UPDATE db_connections SET password_enc = (SELECT password_enc FROM db_connections WHERE id = $1)
			WHERE id = $2`, a, b); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.Pool(ctx, b); err == nil {
			t.Error("password_enc milik koneksi lain tidak boleh bisa dipakai")
		}
	})
}

func TestCheck(t *testing.T) {
	testdb.Fresh(t, "dbconn_check")
	if err := Check(context.Background(), targetParams(t, "it_dbconn_check")); err != nil {
		t.Errorf("target hidup: %v", err)
	}
	bad := targetParams(t, "it_dbconn_check")
	bad.Port = 1
	if err := Check(context.Background(), bad); err == nil {
		t.Error("target mati harus error")
	}
}
