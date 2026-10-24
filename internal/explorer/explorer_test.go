package explorer

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func seededDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testdb.Fresh(t, "explorer")
	seed, err := os.ReadFile("../../scripts/target-seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), string(seed)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

func TestExplorer(t *testing.T) {
	db := seededDB(t)
	ctx := context.Background()

	t.Run("daftar schema dan tabel", func(t *testing.T) {
		schemas, err := ListSchemas(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]string{}
		for _, s := range schemas {
			if strings.HasPrefix(s.Name, "pg_") || s.Name == "information_schema" {
				t.Errorf("schema sistem ikut tampil: %s", s.Name)
			}
			for _, tb := range s.Tables {
				got[s.Name] = append(got[s.Name], tb.Name)
			}
		}
		if strings.Join(got["flight_ops"], ",") != "aircraft,airports,flight_events,flights" {
			t.Errorf("tabel flight_ops = %v", got["flight_ops"])
		}
		if strings.Join(got["maintenance"], ",") != "maintenance_records" {
			t.Errorf("tabel maintenance = %v", got["maintenance"])
		}
	})

	t.Run("struktur tabel", func(t *testing.T) {
		d, err := DescribeTable(ctx, db, "flight_ops", "flights")
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Columns) != 10 || d.Columns[0].Name != "id" || d.Columns[0].Type != "bigint" || d.Columns[0].Nullable {
			t.Errorf("kolom = %+v", d.Columns)
		}
		if c := d.Columns[7]; c.Name != "actual_departure" || c.Type != "timestamp with time zone" || !c.Nullable {
			t.Errorf("kolom actual_departure = %+v", c)
		}
		types := map[string]bool{}
		for _, c := range d.Constraints {
			types[c.Type] = true
		}
		for _, want := range []string{"PRIMARY KEY", "UNIQUE", "FOREIGN KEY", "CHECK"} {
			if !types[want] {
				t.Errorf("constraint %s tidak ada: %+v", want, d.Constraints)
			}
		}
		if d.Constraints[0].Type != "PRIMARY KEY" {
			t.Errorf("PRIMARY KEY harus pertama, dapat %s", d.Constraints[0].Type)
		}
		var hasIdx bool
		for _, ix := range d.Indexes {
			hasIdx = hasIdx || ix.Name == "flights_scheduled_departure_idx"
		}
		if !hasIdx {
			t.Errorf("index flights_scheduled_departure_idx tidak ada: %+v", d.Indexes)
		}
	})

	t.Run("flight_events sengaja belum punya index flight_id", func(t *testing.T) {
		d, err := DescribeTable(ctx, db, "flight_ops", "flight_events")
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Indexes) != 1 || d.Indexes[0].Name != "flight_events_pkey" {
			t.Errorf("index flight_events = %+v; skenario demo butuh hanya primary key", d.Indexes)
		}
	})

	t.Run("paginasi data", func(t *testing.T) {
		for page, want := range map[int]struct {
			rows    int
			hasNext bool
			firstID string
		}{1: {50, true, "1"}, 2: {50, true, "51"}, 3: {20, false, "101"}, 4: {0, false, ""}} {
			p, err := ReadData(ctx, db, "flight_ops", "flights", page)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Rows) != want.rows || p.HasNext != want.hasNext {
				t.Errorf("halaman %d: %d baris, hasNext=%v; mau %d, %v", page, len(p.Rows), p.HasNext, want.rows, want.hasNext)
			}
			if want.rows > 0 && p.Rows[0][0].Value != want.firstID {
				t.Errorf("halaman %d: id pertama %q, mau %q (urut PK)", page, p.Rows[0][0].Value, want.firstID)
			}
		}
	})

	t.Run("NULL dibedakan dari string kosong", func(t *testing.T) {
		p, err := ReadData(ctx, db, "flight_ops", "flights", 3)
		if err != nil {
			t.Fatal(err)
		}
		last := p.Rows[len(p.Rows)-1]
		if last[0].Value != "120" || !last[7].Null || !last[8].Null || last[9].Null || last[9].Value != "scheduled" {
			t.Errorf("penerbangan terakhir (belum berangkat) = %+v", last)
		}
	})

	t.Run("tabel tidak ada dan percobaan injeksi", func(t *testing.T) {
		for _, name := range []string{"nope", `flights"; DROP TABLE flight_ops.airports; --`} {
			if _, err := ReadData(ctx, db, "flight_ops", name, 1); !errors.Is(err, ErrNotFound) {
				t.Errorf("ReadData(%q) err = %v, mau ErrNotFound", name, err)
			}
			if _, err := DescribeTable(ctx, db, "flight_ops", name); !errors.Is(err, ErrNotFound) {
				t.Errorf("DescribeTable(%q) err = %v, mau ErrNotFound", name, err)
			}
		}
	})

	t.Run("koneksi target mati", func(t *testing.T) {
		dead, err := pgxpool.New(ctx, "postgres://x:x@127.0.0.1:1/x?connect_timeout=1")
		if err != nil {
			t.Fatal(err)
		}
		defer dead.Close()
		if _, err := ListSchemas(ctx, dead); err == nil {
			t.Error("harus error saat target tidak bisa dihubungi")
		}
	})
}
