package migration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/sqlpolicy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pg "github.com/pganalyze/pg_query_go/v6"
	parser "github.com/wasilibs/go-pgquery"
)

type PlanNode struct {
	Depth       int
	NodeType    string     `json:"Node Type"`
	JoinType    string     `json:"Join Type"`
	Strategy    string     `json:"Strategy"`
	Relation    string     `json:"Relation Name"`
	Alias       string     `json:"Alias"`
	Index       string     `json:"Index Name"`
	Cond        string     `json:"Index Cond"`
	HashCond    string     `json:"Hash Cond"`
	JoinFilter  string     `json:"Join Filter"`
	Filter      string     `json:"Filter"`
	StartupCost float64    `json:"Startup Cost"`
	TotalCost   float64    `json:"Total Cost"`
	PlanRows    float64    `json:"Plan Rows"`
	ActualTime  float64    `json:"Actual Total Time"`
	ActualRows  float64    `json:"Actual Rows"`
	ActualLoops float64    `json:"Actual Loops"`
	RowsRemoved float64    `json:"Rows Removed by Filter"`
	Plans       []PlanNode `json:"Plans"`
}

type StatementPlan struct {
	N             int
	SQL           string
	Explained     bool
	Nodes         []PlanNode
	PlanningTime  float64
	ExecutionTime float64
}

type Plan struct {
	Analyze    bool
	Statements []StatementPlan
}

func flatten(n PlanNode, depth int, out []PlanNode) []PlanNode {
	n.Depth = depth
	children := n.Plans
	n.Plans = nil
	out = append(out, n)
	for _, c := range children {
		out = flatten(c, depth+1, out)
	}
	return out
}

func Explain(ctx context.Context, target *pgxpool.Pool, sql string, analyze bool) (*Plan, error) {
	if err := CheckTransactional(sql); err != nil {
		return nil, err
	}
	tx, err := target.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTargetUnavailable, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	setup := fmt.Sprintf(`SET LOCAL lock_timeout = %d; SET LOCAL statement_timeout = %d; SET LOCAL search_path = pg_catalog, public; SET LOCAL standard_conforming_strings = on`, LockTimeout.Milliseconds(), StatementTimeout.Milliseconds())
	if _, err := tx.Exec(ctx, setup); err != nil {
		return nil, ValidationError(errorMessage(err))
	}
	tree, err := sqlpolicy.Parse(sql)
	if err != nil {
		return nil, ValidationError(err.Error())
	}

	plan := &Plan{Analyze: analyze}
	raw, _ := split(sql)
	for i, st := range raw {
		sp := StatementPlan{N: i + 1, SQL: st}
		node := tree.Stmts[i].Stmt
		sp.Explained = node.GetSelectStmt() != nil || node.GetInsertStmt() != nil || node.GetUpdateStmt() != nil || node.GetDeleteStmt() != nil || node.GetMergeStmt() != nil
		if sp.Explained {
			if err := safePlan(ctx, tx, node, analyze); err != nil {
				return nil, err
			}

			var out []byte
			safeSQL, err := parser.Deparse(&pg.ParseResult{Stmts: []*pg.RawStmt{{Stmt: node}}})
			if err != nil {
				return nil, ValidationError("SQL tidak dapat disiapkan untuk analisis aman.")
			}
			q := fmt.Sprintf("EXPLAIN (FORMAT JSON, ANALYZE %t) %s", analyze, safeSQL)
			if err := tx.QueryRow(ctx, q).Scan(&out); err != nil {
				return nil, ValidationError(fmt.Sprintf("Statement #%d gagal: %s", i+1, errorMessage(err)))
			}
			var res []struct {
				Plan          PlanNode `json:"Plan"`
				PlanningTime  float64  `json:"Planning Time"`
				ExecutionTime float64  `json:"Execution Time"`
			}
			if err := json.Unmarshal(out, &res); err != nil || len(res) == 0 {
				return nil, fmt.Errorf("parse explain: %v", err)
			}
			sp.Nodes = flatten(res[0].Plan, 0, nil)
			sp.PlanningTime, sp.ExecutionTime = res[0].PlanningTime, res[0].ExecutionTime
		}
		plan.Statements = append(plan.Statements, sp)
	}
	return plan, nil
}
