package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func TestRedactSQL(t *testing.T) {
	for in, want := range map[string]string{
		`ALTER ROLE app PASSWORD 'rahasia123'`:               `ALTER ROLE app PASSWORD '?'`,
		`INSERT INTO t VALUES ('a''b', 'c')`:                 `INSERT INTO t VALUES ('?', '?')`,
		`DO $$ BEGIN PERFORM 'x'; END $$`:                    `DO $$?$$`,
		`CREATE FUNCTION f() AS $body$ secret $body$ LANG`:   `CREATE FUNCTION f() AS $body$?$body$ LANG`,
		`SELECT id FROM accounts WHERE id > $1 AND id <= $2`: `SELECT id FROM accounts WHERE id > $1 AND id <= $2`,
	} {
		if got := RedactSQL(in); got != want {
			t.Errorf("RedactSQL(%q) = %q, mau %q", in, got, want)
		}
	}
	if got := RedactSQL(strings.Repeat("x", 5000)); len(got) > maxQueryLog+4 {
		t.Errorf("query panjang harus dipotong, len=%d", len(got))
	}
}

func TestRedactArgs(t *testing.T) {
	got := RedactArgs([]any{int64(5), true, nil, "password", []byte("x"), time.Unix(0, 0)})
	want := []string{"5", "true", "NULL", "[redacted]", "[redacted]", "1970-01-01T00:00:00Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, mau %v", got, want)
	}
}

func tracedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg := testdb.Fresh(t, "telemetry").Config().Copy()
	pool, err := pgxpool.NewWithConfig(context.Background(), Install(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log bukan JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func find(recs []map[string]any, event string, match func(map[string]any) bool) map[string]any {
	for _, r := range recs {
		if r["event"] == event && (match == nil || match(r)) {
			return r
		}
	}
	return nil
}

func TestPgxTracerLogsEverySQLWithRedaction(t *testing.T) {
	pool := tracedPool(t)
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	buf := capture(t)
	ctx := With(context.Background(), Info{Actor: "7", Database: "db-1", CorrelationID: "corr-1", RevisionID: "revision-1", JobID: 42, Attempt: 2, Batch: 3})

	var s string
	if err := pool.QueryRow(ctx, `SELECT $1::text, 'literal-rahasia'`, "param-rahasia").Scan(&s, new(string)); err != nil {
		t.Fatal(err)
	}
	tx, _ := pool.Begin(ctx)
	tx.Exec(ctx, `CREATE TABLE t (id int)`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx2, _ := pool.Begin(ctx)
	tx2.Rollback(ctx)
	if _, err := pool.Exec(ctx, `SELECT * FROM tidak_ada`); err == nil {
		t.Fatal("harus gagal")
	}

	raw := buf.String()
	if strings.Contains(raw, "literal-rahasia") || strings.Contains(raw, "param-rahasia") {
		t.Fatalf("literal dan parameter sensitif tidak boleh muncul di log:\n%s", raw)
	}
	recs := records(t, buf)
	start := find(recs, "sql.start", func(m map[string]any) bool { return strings.Contains(m["query"].(string), "SELECT $1") })
	if start == nil || start["status"] != "started" || start["job_id"].(float64) != 42 || start["correlation_id"] != "corr-1" ||
		start["actor"] != "7" || start["database"] != "db-1" || start["revision_id"] != "revision-1" ||
		start["attempt"].(float64) != 2 || start["batch"].(float64) != 3 {
		t.Fatalf("event mulai tidak lengkap: %v", start)
	}
	if p := start["params"].([]any); len(p) != 1 || p[0] != "[redacted]" {
		t.Errorf("parameter harus disamarkan: %v", p)
	}
	for _, ev := range []string{"sql.begin.start", "sql.commit.start", "sql.rollback.start"} {
		if find(recs, ev, nil) == nil {
			t.Errorf("event %s harus tercatat terpisah", ev)
		}
	}
	res := find(recs, "sql.result", func(m map[string]any) bool { return m["status"] == "ok" })
	if res == nil || res["duration_ms"] == nil {
		t.Errorf("hasil query harus punya durasi: %v", res)
	}
	failed := find(recs, "sql.result", func(m map[string]any) bool { return m["status"] == "error" })
	if failed == nil || failed["sqlstate"] != "42P01" || failed["level"] != "WARN" {
		t.Errorf("kegagalan harus memuat SQLSTATE: %v", failed)
	}

	var gotErr, gotOK bool
	for _, sp := range sr.Ended() {
		if sp.Name() != "db.query" {
			continue
		}
		gotOK = true
		if sp.Status().Code == codes.Error {
			gotErr = true
		}
	}
	if !gotOK || !gotErr {
		t.Errorf("setiap query harus menjadi span db.query, dan kegagalan menandai error (span=%v err=%v)", gotOK, gotErr)
	}
}

func TestRedactErrorPreservesSQLStateWithoutServerValues(t *testing.T) {
	data := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type integer: "rahasia123"`}
	if got := RedactError(data); strings.Contains(got, "rahasia123") || !strings.Contains(got, "22P02") {
		t.Errorf("nilai input harus disamarkan, pesan tetap terbaca: %q", got)
	}
	other := &pgconn.PgError{Code: "42P01", Message: `relation "tidak_ada" does not exist`}
	if got := RedactError(other); strings.Contains(got, "tidak_ada") || !strings.Contains(got, "42P01") {
		t.Errorf("error harus mempertahankan SQLSTATE tanpa nilai server: %q", got)
	}
	if RedactError(nil) != "" {
		t.Error("nil harus menghasilkan string kosong")
	}
}

func TestTracerDoesNotLeakInputValuesThroughErrorsInLogsOrSpans(t *testing.T) {
	pool := tracedPool(t)
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	buf := capture(t)
	if _, err := pool.Exec(context.Background(), `SELECT 'rahasia123'::int`); err == nil {
		t.Fatal("harus gagal")
	}
	if strings.Contains(buf.String(), "rahasia123") {
		t.Fatalf("log tidak boleh memuat nilai input dari pesan error:\n%s", buf.String())
	}
	for _, sp := range sr.Ended() {
		if strings.Contains(sp.Status().Description, "rahasia123") {
			t.Errorf("status span memuat nilai input: %q", sp.Status().Description)
		}
		for _, ev := range sp.Events() {
			for _, a := range ev.Attributes {
				if strings.Contains(a.Value.Emit(), "rahasia123") {
					t.Errorf("event span memuat nilai input: %s=%s", a.Key, a.Value.Emit())
				}
			}
		}
	}
}

func TestQuietQueriesAreDebugWithoutSpansButErrorsStayVisible(t *testing.T) {
	pool := tracedPool(t)
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	buf := capture(t)

	quiet := Quiet(context.Background())
	if _, err := pool.Exec(quiet, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(quiet, `SELECT * FROM tidak_ada`); err == nil {
		t.Fatal("harus gagal")
	}
	if _, err := pool.Exec(context.Background(), `SELECT 2`); err != nil {
		t.Fatal(err)
	}

	recs := records(t, buf)
	one := find(recs, "sql.start", func(m map[string]any) bool { return m["query"] == "SELECT 1" })
	if one == nil || one["level"] != "DEBUG" {
		t.Errorf("query polling harus berlevel DEBUG: %v", one)
	}
	two := find(recs, "sql.start", func(m map[string]any) bool { return m["query"] == "SELECT 2" })
	if two == nil || two["level"] != "INFO" {
		t.Errorf("query biasa tetap INFO: %v", two)
	}
	if failed := find(recs, "sql.result", func(m map[string]any) bool { return m["status"] == "error" }); failed == nil || failed["level"] != "WARN" {
		t.Errorf("kegagalan query senyap tetap harus terlihat sebagai WARN: %v", failed)
	}
	spans := 0
	for _, sp := range sr.Ended() {
		if sp.Name() == "db.query" {
			spans++
		}
	}
	if spans != 1 {
		t.Errorf("hanya query biasa yang menjadi span, dapat %d", spans)
	}
}

func TestMetricsExposeQueueAndJobCounts(t *testing.T) {
	db := testdb.Fresh(t, "telemetry_metrics")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	conn := uuid.New()
	if _, err := db.Exec(ctx, `INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc) VALUES ($1,'A','h',5432,'d','u','\x00')`, conn); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b"} {
		if _, err := db.Exec(ctx, `INSERT INTO jobs (connection_id, request_id, idempotency_key, root_job_id, plan) VALUES ($1, $2, $3, 0, '{}')`,
			conn, uuid.New(), k); err != nil {
			t.Fatal(err)
		}
	}
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(db, reg)
	if err != nil {
		t.Fatal(err)
	}
	m.QueueWait(2 * time.Second)
	m.LockWait(time.Second)
	m.Retried()
	m.BatchCommitted(100)
	m.Finished("success", 3*time.Second)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]float64{}
	for _, f := range families {
		for _, mt := range f.GetMetric() {
			key := f.GetName()
			for _, l := range mt.GetLabel() {
				key += "{" + l.GetName() + "=" + l.GetValue() + "}"
			}
			switch {
			case mt.GetGauge() != nil:
				byName[key] = mt.GetGauge().GetValue()
			case mt.GetCounter() != nil:
				byName[key] = mt.GetCounter().GetValue()
			case mt.GetHistogram() != nil:
				byName[key] = float64(mt.GetHistogram().GetSampleCount())
			}
		}
	}
	for key, want := range map[string]float64{
		"if3110_jobs{status=queued}":                         2,
		"if3110_jobs{status=running}":                        0,
		"if3110_jobs{status=failed}":                         0,
		"if3110_job_retries_total":                           1,
		"if3110_batches_committed_total":                     1,
		"if3110_rows_committed_total":                        100,
		"if3110_queue_wait_seconds":                          1,
		"if3110_lock_wait_seconds":                           1,
		"if3110_migration_duration_seconds{outcome=success}": 1,
	} {
		if got, ok := byName[key]; !ok || got != want {
			t.Errorf("%s = %v (ada=%v), mau %v", key, got, ok, want)
		}
	}
	// label dibatasi: job ID, user ID, dan SQL tidak boleh menjadi label.
	for key := range byName {
		if strings.Contains(key, "job_id") || strings.Contains(key, "user") || strings.Contains(key, "sql") {
			t.Errorf("label berkardinalitas tinggi: %s", key)
		}
	}
}
