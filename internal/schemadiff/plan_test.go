package schemadiff

import (
	"strings"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

func TestCompare(t *testing.T) {
	base := Snapshot{DBMS: dbconn.PostgreSQL, Schema: "live", Tables: []Table{{Name: "items", Columns: []Column{{Name: "id", Type: "integer"}}}}}
	if p, err := Compare(base, base); err != nil || len(p.Changes) != 0 || len(p.Steps) != 0 {
		t.Fatal("identical schema changed", err)
	}
	t.Run("add column is not destructive", func(t *testing.T) {
		desired := base
		desired.Tables = []Table{{Name: "items", Columns: []Column{{Name: "id", Type: "integer"}, {Name: "note", Type: "text", Nullable: true}}}}
		p, err := Compare(base, desired)
		if err != nil || p.Destructive || len(p.Manual) > 0 || len(p.Steps) != 1 {
			t.Fatal("safe additive change", err)
		}
	})
	t.Run("quoted identifiers and destructive flag", func(t *testing.T) {
		desired := Snapshot{DBMS: dbconn.PostgreSQL, Schema: "ref", Tables: []Table{{Name: `items"; DROP SCHEMA public;--`, Columns: []Column{{Name: `c"quote`, Type: "text", Default: "'literal;value'", Nullable: true}}}}}
		p, err := Compare(base, desired)
		if err != nil || !p.Destructive || !strings.Contains(p.SQL(), `"items""; DROP SCHEMA public;--"`) {
			t.Fatal("identifier quoting", p.SQL(), err)
		}
		if strings.Contains(p.SQL(), "CASCADE") {
			t.Fatal("must not use DROP CASCADE")
		}
	})
	t.Run("cross dialect rejected", func(t *testing.T) {
		other := base
		other.DBMS = dbconn.MariaDB
		if _, err := Compare(base, other); err == nil {
			t.Fatal("cross dialect accepted")
		}
	})
	t.Run("column order PostgreSQL requires manual review", func(t *testing.T) {
		desired := base
		desired.Tables = []Table{{Name: "items", Columns: []Column{{Name: "first", Type: "integer"}, {Name: "id", Type: "integer"}}}}
		p, err := Compare(base, desired)
		if err != nil || len(p.Manual) == 0 {
			t.Fatal("unsafe reorder accepted", err)
		}
	})
	t.Run("sequence changes require manual review", func(t *testing.T) {
		desired := base
		desired.Tables = []Table{{Name: "items", Columns: []Column{{Name: "id", Type: "integer", Auto: "always"}}}}
		p, err := Compare(base, desired)
		if err != nil || len(p.Manual) == 0 {
			t.Fatal("identity conversion generated automatically", err)
		}
	})
	t.Run("unsupported metadata blocks automatic draft", func(t *testing.T) {
		desired := base
		desired.Tables = []Table{{Name: "items", Columns: []Column{{Name: "id", Type: "integer"}, {Name: "computed", Type: "integer", Manual: "generated"}}}}
		p, err := Compare(base, desired)
		if err != nil || len(p.Manual) == 0 {
			t.Fatal("generated column silently ignored", err)
		}
	})
	t.Run("stable output", func(t *testing.T) {
		desired := base
		desired.Tables = []Table{{Name: "z", Columns: []Column{{Name: "id", Type: "integer"}}}, {Name: "a", Columns: []Column{{Name: "id", Type: "integer"}}}}
		first, _ := Compare(base, desired)
		for range 30 {
			next, _ := Compare(base, desired)
			if next.SQL() != first.SQL() {
				t.Fatal("nondeterministic SQL")
			}
		}
	})
}

func TestForeignKeyScope(t *testing.T) {
	id := Column{Name: "id", Type: "integer"}
	parent := Table{Name: "parent", Columns: []Column{id}, Constraints: []Constraint{{Name: "parent_pkey", Kind: "PRIMARY KEY", Columns: []string{"id"}}}}
	child := Table{Name: "child", Columns: []Column{id, {Name: "pid", Type: "integer", Nullable: true}},
		Constraints: []Constraint{{Name: "child_fk", Kind: "FOREIGN KEY", Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"}}}}
	other := Table{Name: "other", Columns: []Column{id}}
	withNote := other
	withNote.Columns = []Column{id, {Name: "note", Type: "text", Nullable: true}}
	for _, kind := range []dbconn.DBMS{dbconn.PostgreSQL, dbconn.MariaDB} {
		current := Snapshot{DBMS: kind, Schema: "app", Tables: []Table{child, other, parent}}
		t.Run(string(kind)+" unrelated change keeps foreign keys", func(t *testing.T) {
			desired := Snapshot{DBMS: kind, Schema: "ref", Tables: []Table{child, withNote, parent}}
			p, _ := Compare(current, desired)
			if len(p.Steps) != 1 || strings.Contains(p.SQL(), "child_fk") || strings.Contains(p.SQL(), "MODIFY") {
				t.Fatal("only the added column expected:\n", p.SQL())
			}
		})
		t.Run(string(kind)+" unrelated key on referenced table keeps foreign key", func(t *testing.T) {
			keyed := parent
			keyed.Columns = []Column{id, {Name: "code", Type: "text", Nullable: true}}
			keyed.Constraints = append([]Constraint{{Name: "parent_code_key", Kind: "UNIQUE", Columns: []string{"code"}}}, parent.Constraints...)
			desired := Snapshot{DBMS: kind, Schema: "ref", Tables: []Table{child, other, keyed}}
			p, _ := Compare(current, desired)
			if strings.Contains(p.SQL(), "child_fk") {
				t.Fatal("unique key on another column must not touch the foreign key:\n", p.SQL())
			}
		})
		t.Run(string(kind)+" referenced column change recreates foreign key", func(t *testing.T) {
			bigParent := parent
			bigParent.Columns = []Column{{Name: "id", Type: "bigint"}}
			desired := Snapshot{DBMS: kind, Schema: "ref", Tables: []Table{child, other, bigParent}}
			p, _ := Compare(current, desired)
			sql := p.SQL()
			drop, add := strings.Index(sql, "child_fk"), strings.LastIndex(sql, "child_fk")
			if drop < 0 || drop == add || !strings.Contains(sql[add-40:], "ADD CONSTRAINT") {
				t.Fatal("foreign key must be dropped before and re-added after the change:\n", sql)
			}
		})
	}
}

func FuzzQuote(f *testing.F) {
	for _, s := range []string{"normal", `a"b`, "a`b", "a;DROP TABLE x;--", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, kind := range []dbconn.DBMS{dbconn.PostgreSQL, dbconn.MariaDB} {
			q := quote(kind, s)
			delimiter := q[:1]
			inner := q[1 : len(q)-1]
			decoded := strings.ReplaceAll(inner, delimiter+delimiter, delimiter)
			if decoded != s {
				t.Fatal("quote roundtrip")
			}
		}
	})
}
