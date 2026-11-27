package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	logSDK "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	traceSDK "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type HTTP struct {
	Registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	tracer   trace.Tracer
	shutdown func(context.Context) error
}

type fanout struct{ handlers []slog.Handler }

func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	var result error
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			result = errors.Join(result, h.Handle(ctx, r))
		}
	}
	return result
}
func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	list := []slog.Handler{}
	for _, h := range f.handlers {
		list = append(list, h.WithAttrs(attrs))
	}
	return fanout{list}
}
func (f fanout) WithGroup(name string) slog.Handler {
	list := []slog.Handler{}
	for _, h := range f.handlers {
		list = append(list, h.WithGroup(name))
	}
	return fanout{list}
}

func New(ctx context.Context, platform *pgxpool.Pool, targets *dbconn.Registry) (*HTTP, error) {
	name := os.Getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = "if3110-api"
	}
	res := resource.NewWithAttributes("", attribute.String("service.name", name), attribute.String("service.version", "2.0.0"))
	var exporter traceSDK.SpanExporter
	var err error
	remote := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	if remote {
		exporter, err = otlptracehttp.New(ctx)
	} else {
		exporter, err = stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
	}
	if err != nil {
		return nil, err
	}
	tp := traceSDK.NewTracerProvider(traceSDK.WithResource(res), traceSDK.WithBatcher(exporter), traceSDK.WithSampler(traceSDK.ParentBased(traceSDK.AlwaysSample())))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	handlers := []slog.Handler{slog.NewJSONHandler(os.Stdout, nil)}
	var lp *logSDK.LoggerProvider
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != "" {
		e, err := otlploghttp.New(ctx)
		if err != nil {
			tp.Shutdown(ctx)
			return nil, err
		}
		lp = logSDK.NewLoggerProvider(logSDK.WithResource(res), logSDK.WithProcessor(logSDK.NewBatchProcessor(e)))
		handlers = append(handlers, otelslog.NewHandler(name, otelslog.WithLoggerProvider(lp)))
	}
	slog.SetDefault(slog.New(fanout{handlers}))
	registry := prometheus.NewRegistry()
	h := &HTTP{Registry: registry, tracer: tp.Tracer("if3110/http"), requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "if3110_http_requests_total", Help: "HTTP responses by method and status."}, []string{"method", "status"}), duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "if3110_http_duration_seconds", Help: "HTTP request duration."}, []string{"method"})}
	registry.MustRegister(h.requests, h.duration, prometheus.NewGoCollector(), &poolCollector{platform: platform, targets: targets})
	h.shutdown = func(ctx context.Context) error {
		var e error
		if lp != nil {
			e = lp.Shutdown(ctx)
		}
		return errors.Join(e, tp.Shutdown(ctx))
	}
	return h, nil
}

func (h *HTTP) Shutdown(ctx context.Context) error { return h.shutdown(ctx) }
func (h *HTTP) Metrics() http.Handler              { return promhttp.HandlerFor(h.Registry, promhttp.HandlerOpts{}) }

type response struct {
	http.ResponseWriter
	status int
}

func (w *response) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *response) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *response) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (h *HTTP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		method := r.Method
		switch method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		default:
			method = "OTHER"
		}
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := h.tracer.Start(ctx, "http.request", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("http.request.method", method)))
		defer span.End()
		start := time.Now()
		rw := &response{ResponseWriter: w}
		next.ServeHTTP(rw, r.WithContext(ctx))
		status := rw.status
		if status == 0 {
			status = 200
		}
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		h.requests.WithLabelValues(method, strconv.Itoa(status)).Inc()
		h.duration.WithLabelValues(method).Observe(time.Since(start).Seconds())
	})
}

type poolCollector struct {
	platform *pgxpool.Pool
	targets  *dbconn.Registry
}

var poolConnections = prometheus.NewDesc("if3110_pool_connections", "Active and idle connections.", []string{"pool", "dbms", "state"}, nil)
var poolWait = prometheus.NewDesc("if3110_pool_wait_seconds_total", "Cumulative time waiting for an available connection.", []string{"pool", "dbms"}, nil)
var poolCount = prometheus.NewDesc("if3110_pool_wait_count_total", "Connection acquisitions that waited for availability.", []string{"pool", "dbms"}, nil)

func (p *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolConnections
	ch <- poolWait
	ch <- poolCount
}
func (p *poolCollector) Collect(ch chan<- prometheus.Metric) {
	emit := func(kind string, s dbconn.PoolSnapshot) {
		ch <- prometheus.MustNewConstMetric(poolConnections, prometheus.GaugeValue, s.Active, kind, s.DBMS, "active")
		ch <- prometheus.MustNewConstMetric(poolConnections, prometheus.GaugeValue, s.Idle, kind, s.DBMS, "idle")
		ch <- prometheus.MustNewConstMetric(poolWait, prometheus.CounterValue, s.WaitSeconds, kind, s.DBMS)
		ch <- prometheus.MustNewConstMetric(poolCount, prometheus.CounterValue, s.WaitCount, kind, s.DBMS)
	}
	if p.platform != nil {
		s := p.platform.Stat()
		emit("platform", dbconn.PoolSnapshot{DBMS: "postgresql", Active: float64(s.AcquiredConns()), Idle: float64(s.IdleConns()), WaitSeconds: s.EmptyAcquireWaitTime().Seconds(), WaitCount: float64(s.EmptyAcquireCount())})
	}
	byDBMS := map[string]dbconn.PoolSnapshot{}
	if p.targets != nil {
		for _, s := range p.targets.Stats() {
			a := byDBMS[s.DBMS]
			a.DBMS = s.DBMS
			a.Active += s.Active
			a.Idle += s.Idle
			a.WaitSeconds += s.WaitSeconds
			a.WaitCount += s.WaitCount
			byDBMS[s.DBMS] = a
		}
	}
	for _, s := range byDBMS {
		emit("target", s)
	}
}
