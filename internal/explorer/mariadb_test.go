package explorer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func TestIntegrationMariaExplorer(t *testing.T) {
	db, name := testdb.FreshMaria(t, "maria_explorer")
	target := &dbconn.Target{DBMS: dbconn.MariaDB, Database: name, Maria: db}
	ctx := context.Background()
	for _, q := range []string{
		"CREATE TABLE parents(id INT PRIMARY KEY) ENGINE=InnoDB",
		"CREATE TABLE samples(id INT PRIMARY KEY, parent_id INT, name VARCHAR(255) UNIQUE, note TEXT, amount INT CHECK(amount>=0), CONSTRAINT fk_parent FOREIGN KEY(parent_id) REFERENCES parents(id)) ENGINE=InnoDB",
		"CREATE INDEX idx_amount ON samples(amount,id)",
		"CREATE VIEW sample_view AS SELECT id,name FROM samples",
		"CREATE TABLE `odd``name` (`odd``column` INT PRIMARY KEY)",
		"INSERT INTO parents VALUES (1)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 120; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO samples VALUES (?,1,?,NULL,?)", i, fmt.Sprintf("name-%03d", i), i); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("schema scoped to configured database", func(t *testing.T) {
		schemas, err := ListTargetSchemas(ctx, target)
		if err != nil || len(schemas) != 1 || schemas[0].Name != name || len(schemas[0].Tables) != 4 {
			t.Fatalf("schemas=%+v err=%v", schemas, err)
		}
	})
	t.Run("columns constraints indexes", func(t *testing.T) {
		d, err := DescribeTargetTable(ctx, target, name, "samples")
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Columns) != 5 || d.Columns[0].Nullable {
			t.Fatalf("columns %+v", d.Columns)
		}
		types := map[string]bool{}
		for _, c := range d.Constraints {
			types[c.Type] = true
		}
		for _, kind := range []string{"PRIMARY KEY", "FOREIGN KEY", "CHECK", "UNIQUE"} {
			if !types[kind] {
				t.Fatalf("missing %s", kind)
			}
		}
		found := false
		for _, ix := range d.Indexes {
			found = found || ix.Name == "idx_amount"
		}
		if !found {
			t.Fatal("index missing")
		}
	})
	t.Run("stable pagination and null", func(t *testing.T) {
		for page, want := range map[int]int{1: 50, 2: 50, 3: 20, 4: 0} {
			p, err := ReadTargetData(ctx, target, name, "samples", page)
			if err != nil || len(p.Rows) != want || p.HasNext != (page < 3) {
				t.Fatalf("page %d: %+v %v", page, p, err)
			}
			if want > 0 && (p.Rows[0][0].Value != fmt.Sprint((page-1)*50+1) || !p.Rows[0][3].Null) {
				t.Fatal("ordering/null")
			}
		}
	})
	t.Run("quoted identifiers and injection", func(t *testing.T) {
		if _, err := ReadTargetData(ctx, target, name, "odd`name", 1); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"absent", "samples`; DROP TABLE parents;--"} {
			if _, err := ReadTargetData(ctx, target, name, table, 1); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
		}
		if _, err := ReadTargetData(ctx, target, "mysql", "user", 1); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-schema access", err)
		}
	})
	t.Run("view and truncation", func(t *testing.T) {
		if _, err := ReadTargetData(ctx, target, name, "sample_view", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "UPDATE samples SET note=? WHERE id=1", strings.Repeat("é", 250)); err != nil {
			t.Fatal(err)
		}
		p, err := ReadTargetData(ctx, target, name, "samples", 1)
		if err != nil || len([]rune(p.Rows[0][3].Value)) != 201 {
			t.Fatal("truncation", err)
		}
	})
	t.Run("empty string differs from NULL", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE samples SET note='' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		p, err := ReadTargetData(ctx, target, name, "samples", 1)
		if err != nil || p.Rows[0][3].Null || p.Rows[0][3].Value != "" || !p.Rows[1][3].Null {
			t.Fatal("empty/NULL distinction", err)
		}
	})
}
