package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pg "github.com/pganalyze/pg_query_go/v6"
	"github.com/prometheus/client_golang/prometheus"
	parser "github.com/wasilibs/go-pgquery"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxQueryLog = 2000

type Info struct {
	Actor         string
	Database      string
	CorrelationID string
	MigrationID   string
	RevisionID    string
	RequestID     string
	ExecutionID   string
	JobID         int64
	Attempt       int
	Batch         int
	Op            string
}

type infoKey struct{}

func With(ctx context.Context, i Info) context.Context { return context.WithValue(ctx, infoKey{}, i) }

func From(ctx context.Context) Info {
	i, _ := ctx.Value(infoKey{}).(Info)
	return i
}

type quietKey struct{}

func Quiet(ctx context.Context) context.Context { return context.WithValue(ctx, quietKey{}, true) }

func isQuiet(ctx context.Context) bool { q, _ := ctx.Value(quietKey{}).(bool); return q }

func WithBatch(ctx context.Context, n int) context.Context {
	i := From(ctx)
	i.Batch = n
	return With(ctx, i)
}

func (i Info) attrs() []slog.Attr {
	a := []slog.Attr{slog.String("actor", i.Actor), slog.String("database", i.Database), slog.String("correlation_id", i.CorrelationID)}
	if i.JobID != 0 {
		a = append(a, slog.Int64("job_id", i.JobID))
	}
	if i.MigrationID != "" {
		a = append(a, slog.String("migration_id", i.MigrationID))
	}
	if i.RevisionID != "" {
		a = append(a, slog.String("revision_id", i.RevisionID))
	}
	if i.RequestID != "" {
		a = append(a, slog.String("request_id", i.RequestID))
	}
	if i.ExecutionID != "" {
		a = append(a, slog.String("execution_id", i.ExecutionID))
	}
	if i.Attempt != 0 {
		a = append(a, slog.Int("attempt", i.Attempt))
	}
	if i.Batch != 0 {
		a = append(a, slog.Int("batch", i.Batch))
	}
	if i.Op != "" {
		a = append(a, slog.String("op", i.Op))
	}
	return a
}

func Event(ctx context.Context, level slog.Level, event, status string, extra ...slog.Attr) {
	attrs := append([]slog.Attr{slog.String("event", event), slog.String("status", status)}, From(ctx).attrs()...)
	slog.Default().LogAttrs(ctx, level, event, append(attrs, extra...)...)
}

func Tracer() trace.Tracer { return otel.Tracer("if3110/worker") }

func RedactSQL(q string) string {
	if strings.IndexByte(q, 0) >= 0 {
		return "[SQL redacted]"
	}
	scanned, err := parser.Scan(q)
	if err != nil {
		return "[SQL redacted]"
	}
	var b strings.Builder
	last := 0
	for _, token := range scanned.Tokens {
		start, end := int(token.Start), int(token.End)
		if start < last || end < start || end > len(q) {
			return "[SQL redacted]"
		}
		b.WriteString(q[last:start])
		raw := q[start:end]
		switch token.Token {
		case pg.Token_SQL_COMMENT, pg.Token_C_COMMENT:
			b.WriteByte(' ')
		case pg.Token_SCONST, pg.Token_USCONST, pg.Token_BCONST, pg.Token_XCONST:
			if strings.HasPrefix(raw, "$") {
				endTag := strings.IndexByte(raw[1:], '$')
				if endTag >= 0 {
					tag := raw[:endTag+2]
					b.WriteString(tag + "?" + tag)
				} else {
					b.WriteString("'?'")
				}
			} else {
				b.WriteString("'?'")
			}
		case pg.Token_UIDENT:
			b.WriteString("\"?\"")
		case pg.Token_IDENT:
			if strings.HasPrefix(raw, "\"") {
				b.WriteString("\"?\"")
			} else {
				b.WriteString(raw)
			}
		default:
			b.WriteString(raw)
		}
		last = end
	}
	b.WriteString(q[last:])
	out := b.String()
	if len(out) > maxQueryLog {
		out = out[:maxQueryLog] + "..."
	}
	return out
}

func RedactError(err error) string {
	if err == nil {
		return ""
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		code := pg.Code
		if len(code) != 5 || strings.IndexFunc(code, func(r rune) bool { return (r < '0' || r > '9') && (r < 'A' || r > 'Z') }) >= 0 {
			return "Operasi database gagal."
		}
		message := "Database error"
		switch code {
		case "22012":
			message = "division by zero"
		case "57014":
			message = "statement timeout atau query dibatalkan"
		case "55P03":
			message = "lock timeout atau lock belum tersedia"
		}
		return message + " (SQLSTATE " + code + ")."
	}
	var maria *mysql.MySQLError
	if errors.As(err, &maria) {
		return fmt.Sprintf("Operasi MariaDB gagal (kode %d).", maria.Number)
	}
	return "Operasi database gagal."
}

func RedactArgs(args []any) []string {
	out := make([]string, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case nil:
			out[i] = "NULL"
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, bool:
			out[i] = fmt.Sprint(v)
		case time.Time:
			out[i] = v.UTC().Format(time.RFC3339)
		default:
			out[i] = "[redacted]"
		}
	}
	return out
}

type PgxTracer struct{}

type spanKey struct{}
type startKey struct{}

func eventFor(sql string) string {
	switch strings.ToLower(strings.TrimSpace(sql)) {
	case "begin":
		return "sql.begin"
	case "commit":
		return "sql.commit"
	case "rollback":
		return "sql.rollback"
	}
	return "sql"
}

func (PgxTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	level := slog.LevelInfo
	if isQuiet(ctx) {
		level = slog.LevelDebug
	} else {
		var span trace.Span
		ctx, span = Tracer().Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("db.system", "postgresql"), attribute.String("db.statement", RedactSQL(d.SQL))))
		ctx = context.WithValue(ctx, spanKey{}, span)
	}
	ctx = context.WithValue(ctx, startKey{}, time.Now())
	ev := eventFor(d.SQL)
	attrs := append([]slog.Attr{slog.String("event", ev+".start"), slog.String("status", "started"),
		slog.String("query", RedactSQL(d.SQL)), slog.Any("params", RedactArgs(d.Args))}, From(ctx).attrs()...)
	slog.Default().LogAttrs(ctx, level, ev+".start", attrs...)
	return ctx
}

func (PgxTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryEndData) {
	start, _ := ctx.Value(startKey{}).(time.Time)
	span, _ := ctx.Value(spanKey{}).(trace.Span)
	status, level := "ok", slog.LevelInfo
	if isQuiet(ctx) {
		level = slog.LevelDebug
	}
	attrs := []slog.Attr{slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000)}
	if d.CommandTag.String() != "" {
		attrs = append(attrs, slog.Int64("rows", d.CommandTag.RowsAffected()))
	}
	if d.Err != nil {
		status, level = "error", slog.LevelWarn
		attrs = append(attrs, slog.String("error", RedactError(d.Err)))
		var pg *pgconn.PgError
		if errors.As(d.Err, &pg) {
			attrs = append(attrs, slog.String("sqlstate", pg.Code))
		}
	}
	attrs = append([]slog.Attr{slog.String("event", "sql.result"), slog.String("status", status)}, append(attrs, From(ctx).attrs()...)...)
	slog.Default().LogAttrs(ctx, level, "sql.result", attrs...)
	if span != nil {
		if d.Err != nil {
			span.RecordError(errors.New(RedactError(d.Err)))
			span.SetStatus(codes.Error, RedactError(d.Err))
		}
		span.End()
	}
}

func Install(cfg *pgxpool.Config) *pgxpool.Config {
	cfg.ConnConfig.Tracer = PgxTracer{}
	return cfg
}

type Metrics struct {
	queueWait, lockWait, duration *prometheus.HistogramVec
	retries, batches, rows        prometheus.Counter
}

func NewMetrics(db *pgxpool.Pool, reg prometheus.Registerer) (*Metrics, error) {
	buckets := []float64{.01, .05, .1, .5, 1, 5, 15, 60, 300, 1800}
	m := &Metrics{
		queueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "if3110_queue_wait_seconds", Help: "Waktu job menunggu di queue sebelum diklaim worker.", Buckets: buckets}, nil),
		lockWait:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "if3110_lock_wait_seconds", Help: "Waktu menunggu lock database target.", Buckets: buckets}, nil),
		duration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "if3110_migration_duration_seconds", Help: "Durasi eksekusi migration per hasil.", Buckets: buckets}, []string{"outcome"}),
		retries:   prometheus.NewCounter(prometheus.CounterOpts{Name: "if3110_job_retries_total", Help: "Jumlah retry job."}),
		batches:   prometheus.NewCounter(prometheus.CounterOpts{Name: "if3110_batches_committed_total", Help: "Jumlah batch yang committed."}),
		rows:      prometheus.NewCounter(prometheus.CounterOpts{Name: "if3110_rows_committed_total", Help: "Jumlah row yang committed oleh batch."}),
	}
	for _, c := range []prometheus.Collector{m.queueWait, m.lockWait, m.duration, m.retries, m.batches, m.rows, &jobCollector{db}} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Metrics) QueueWait(d time.Duration) { m.queueWait.WithLabelValues().Observe(d.Seconds()) }
func (m *Metrics) LockWait(d time.Duration)  { m.lockWait.WithLabelValues().Observe(d.Seconds()) }
func (m *Metrics) Retried()                  { m.retries.Inc() }
func (m *Metrics) Finished(outcome string, d time.Duration) {
	m.duration.WithLabelValues(outcome).Observe(d.Seconds())
}
func (m *Metrics) BatchCommitted(rows int64) {
	m.batches.Inc()
	m.rows.Add(float64(rows))
}

type jobCollector struct{ db *pgxpool.Pool }

var jobsDesc = prometheus.NewDesc("if3110_jobs", "Jumlah job per status.", []string{"status"}, nil)

func (c *jobCollector) Describe(ch chan<- *prometheus.Desc) { ch <- jobsDesc }

func (c *jobCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(Quiet(context.Background()), 2*time.Second)
	defer cancel()

	counts := map[string]int64{}
	if rows, err := c.db.Query(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`); err == nil {
		for rows.Next() {
			var s string
			var n int64
			if rows.Scan(&s, &n) == nil {
				counts[s] = n
			}
		}
		rows.Close()
	}
	for _, s := range []string{"queued", "running", "retry_wait", "success", "failed", "cancelled"} {
		ch <- prometheus.MustNewConstMetric(jobsDesc, prometheus.GaugeValue, float64(counts[s]), s)
	}
}
