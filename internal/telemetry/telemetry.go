// Package telemetry: hook bersama untuk logging SQL, tracing, dan metrics queue/worker.
// PgxTracer dipasang pada konfigurasi pool mana pun (platform, target, explorer M1) sehingga
// setiap SQL, termasuk read-only, tercatat di log JSON (slog) dan menjadi span OpenTelemetry.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxQueryLog = 2000

// Info: atribut korelasi yang ikut pada setiap log dan span di dalam satu pekerjaan.
type Info struct {
	Actor         string
	Database      string
	CorrelationID string
	MigrationID   string
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

// Quiet menandai query internal yang berulang tanpa henti (polling queue, heartbeat, scrape metrics).
// Query itu tetap tercatat tetapi pada level Debug dan tanpa span, supaya worker yang menganggur
// tidak membanjiri log (±34 ribu baris per jam per worker bila dicatat sebagai Info).
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

// Event mencatat satu kejadian penting (job.claimed, job.retry, lock.acquired, batch.committed, ...).
func Event(ctx context.Context, level slog.Level, event, status string, extra ...slog.Attr) {
	attrs := append([]slog.Attr{slog.String("event", event), slog.String("status", status)}, From(ctx).attrs()...)
	slog.Default().LogAttrs(ctx, level, event, append(attrs, extra...)...)
}

// Tracer mengambil tracer dari provider global (diatur observability.New di proses API/worker).
func Tracer() trace.Tracer { return otel.Tracer("if3110/worker") }

// RedactSQL menyamarkan literal string dan isi dollar-quote, tempat password atau data sensitif
// biasanya ditulis (mis. ALTER ROLE ... PASSWORD '...'), lalu memotong query yang terlalu panjang.
func RedactSQL(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); {
		switch c := q[i]; {
		case c == '\'':
			j := i + 1
			for j < len(q) {
				if q[j] == '\'' {
					if j+1 < len(q) && q[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			b.WriteString("'?'")
			i = j + 1
		case c == '$' && i+1 < len(q) && (q[i+1] == '$' || isTagStart(q[i+1])):
			end := strings.IndexByte(q[i+1:], '$')
			if end < 0 || !validTag(q[i+1:i+1+end]) {
				b.WriteByte(c)
				i++
				continue
			}
			tag := q[i : i+end+2]
			if close := strings.Index(q[i+len(tag):], tag); close >= 0 {
				b.WriteString(tag + "?" + tag)
				i += len(tag) + close + len(tag)
			} else {
				b.WriteString(tag + "?")
				i = len(q)
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	out := b.String()
	if len(out) > maxQueryLog {
		out = out[:maxQueryLog] + "…"
	}
	return out
}

func isTagStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }

func validTag(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isTagStart(s[i]) && !(i > 0 && s[i] >= '0' && s[i] <= '9') {
			return false
		}
	}
	return true
}

var quoted = regexp.MustCompile(`"[^"]*"|'[^']*'`)

// RedactError menyamarkan nilai yang ikut tertulis di pesan error server. Error data (SQLSTATE kelas 22)
// memuat nilai input apa adanya, mis. `invalid input syntax for type integer: "rahasia"`, sehingga isi
// kutipannya disamarkan; error lain (relation "x" does not exist) tetap utuh agar mudah ditelusuri.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && strings.HasPrefix(pg.Code, "22") {
		return quoted.ReplaceAllString(err.Error(), `"?"`)
	}
	return err.Error()
}

// RedactArgs: angka, boolean, dan waktu boleh tampil; teks dan biner disamarkan.
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

// PgxTracer memasang logging dan span pada setiap query pgx. begin, commit, dan rollback ikut
// tercatat terpisah karena pgx menjalankannya sebagai statement.
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
		level = slog.LevelDebug // kegagalan di bawah tetap dinaikkan ke Warn
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

// Install memasang PgxTracer pada konfigurasi pool.
func Install(cfg *pgxpool.Config) *pgxpool.Config {
	cfg.ConnConfig.Tracer = PgxTracer{}
	return cfg
}

// Metrics: metrics queue/migration untuk endpoint Prometheus. Label hanya status/outcome
// (kardinalitas kecil); job ID, user ID, dan SQL tidak pernah menjadi label.
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

// jobCollector membaca jumlah job per status langsung dari tabel jobs saat scrape.
type jobCollector struct{ db *pgxpool.Pool }

var jobsDesc = prometheus.NewDesc("if3110_jobs", "Jumlah job per status.", []string{"status"}, nil)

func (c *jobCollector) Describe(ch chan<- *prometheus.Desc) { ch <- jobsDesc }

func (c *jobCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(Quiet(context.Background()), 2*time.Second)
	defer cancel()
	// Query langsung (bukan lewat paket jobs) agar telemetry tidak membentuk siklus impor dengan dbconn.
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
