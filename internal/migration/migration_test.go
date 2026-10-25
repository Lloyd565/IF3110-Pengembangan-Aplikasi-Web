package migration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

var (
	connA = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	connB = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
)

func platformDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Fresh(t, "migration")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES (1, 'budi', 'x');
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc) VALUES
			('aaaaaaaa-0000-0000-0000-000000000001', 'A', 'h', 5432, 'd', 'u', '\x00'),
			('bbbbbbbb-0000-0000-0000-000000000002', 'B', 'h', 5432, 'd', 'u', '\x00');`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestValidate(t *testing.T) {
	cases := map[string]struct{ name, sql string }{
		"nama kosong":      {"   ", "select 1"},
		"nama kepanjangan": {strings.Repeat("a", 201), "select 1"},
		"sql kosong":       {"m", " \n "},
		"sql kebesaran":    {"m", strings.Repeat("x", 100_001)},
	}
	for label, c := range cases {
		if _, _, err := Validate(c.name, c.sql); !errors.As(err, new(ValidationError)) {
			t.Errorf("%s: err = %v, mau ValidationError", label, err)
		}
	}
	name, _, err := Validate("  tambah kolom  ", "select 1")
	if err != nil || name != "tambah kolom" {
		t.Errorf("valid: name=%q err=%v", name, err)
	}
}

func TestRepository(t *testing.T) {
	db := platformDB(t)
	ctx := context.Background()

	id, err := CreateDraft(ctx, db, connA, 1, " tambah kolom phone ", "ALTER TABLE shop.customers ADD COLUMN phone text;")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("draft baru tanpa versi", func(t *testing.T) {
		m, err := Get(ctx, db, connA, id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status != StatusDraft || m.Version != nil || m.Name != "tambah kolom phone" || m.CreatedByName != "budi" {
			t.Errorf("draft = %+v", m)
		}
	})

	t.Run("migration koneksi lain tidak bisa diakses", func(t *testing.T) {
		if _, err := Get(ctx, db, connB, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get lewat koneksi B: err = %v, mau ErrNotFound", err)
		}
		if err := UpdateDraft(ctx, db, connB, id, "x", "select 1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateDraft lewat koneksi B: err = %v, mau ErrNotFound", err)
		}
	})

	t.Run("ubah draft", func(t *testing.T) {
		if err := UpdateDraft(ctx, db, connA, id, "nama baru", "select 2"); err != nil {
			t.Fatal(err)
		}
		m, _ := Get(ctx, db, connA, id)
		if m.Name != "nama baru" || m.SQL != "select 2" {
			t.Errorf("setelah update = %+v", m)
		}
	})

	t.Run("migration yang sudah dieksekusi tidak bisa diubah", func(t *testing.T) {
		if _, err := db.Exec(ctx, `UPDATE migrations SET status = 'success', version = 1 WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if err := UpdateDraft(ctx, db, connA, id, "x", "select 3"); !errors.Is(err, ErrNotDraft) {
			t.Errorf("err = %v, mau ErrNotDraft", err)
		}
	})

	t.Run("riwayat: draft di atas lalu versi terbaru", func(t *testing.T) {
		if _, err := db.Exec(ctx, `
			INSERT INTO migrations (connection_id, version, name, sql, status, created_by)
			VALUES ($1, 2, 'v2', 'select 1', 'failed', 1)`, connA); err != nil {
			t.Fatal(err)
		}
		draft, err := CreateDraft(ctx, db, connA, 1, "draft", "select 1")
		if err != nil {
			t.Fatal(err)
		}
		list, err := List(ctx, db, connA)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range list {
			got = append(got, m.Name)
		}
		if strings.Join(got, ",") != "draft,v2,nama baru" || list[0].ID != draft {
			t.Errorf("urutan riwayat = %v", got)
		}
		if other, _ := List(ctx, db, connB); len(other) != 0 {
			t.Errorf("riwayat koneksi B harus kosong, dapat %d", len(other))
		}
	})
}
