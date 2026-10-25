package migration

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func TestSplitRaw(t *testing.T) {
	raw, _ := split("CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END $$ LANGUAGE plpgsql;\n-- x;\nINSERT INTO a VALUES ('a;b'); ;")
	want := []string{"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END $$ LANGUAGE plpgsql", "-- x;\nINSERT INTO a VALUES ('a;b')"}
	if !reflect.DeepEqual(raw, want) {
		t.Fatalf("raw = %q", raw)
	}
}

func TestExplainRollsBack(t *testing.T) {
	ctx := context.Background()
	target := testdb.Fresh(t, "migration_explain")
	if _, err := target.Exec(ctx, `CREATE TABLE a (id int PRIMARY KEY); INSERT INTO a SELECT generate_series(1, 1000)`); err != nil {
		t.Fatal(err)
	}
	plan, err := Explain(ctx, target, `CREATE TABLE b (a_id int); INSERT INTO b SELECT id FROM a; SELECT * FROM a JOIN b ON b.a_id = a.id WHERE a.id = 5`, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Statements) != 3 || plan.Statements[0].Explained || !plan.Statements[2].Explained {
		t.Fatalf("statements = %+v", plan.Statements)
	}
	sel := plan.Statements[2]
	var sawIndex bool
	for _, n := range sel.Nodes {
		sawIndex = sawIndex || n.Index == "a_pkey"
	}
	if !sawIndex || sel.Nodes[0].ActualLoops == 0 || sel.ExecutionTime == 0 {
		t.Fatalf("plan tanpa index/analyze: %+v", sel)
	}
	var exists bool
	if err := target.QueryRow(ctx, `SELECT to_regclass('b') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("tabel b harus ikut di-rollback (exists=%v err=%v)", exists, err)
	}

	if _, err := Explain(ctx, target, `SELECT * FROM tidak_ada`, false); !errors.As(err, new(ValidationError)) {
		t.Fatalf("error SQL harus ValidationError, dapat %T %v", err, err)
	}
}
