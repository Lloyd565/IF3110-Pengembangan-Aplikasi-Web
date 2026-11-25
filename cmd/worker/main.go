// Command worker menjalankan job migration dari queue persisten. Proses terpisah dari API: SQL
// migration tidak pernah berjalan di dalam request HTTP.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/observability"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/worker"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

func duration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("%s harus berupa durasi Go yang valid dan positif", name)
	}
	return d
}

func main() {
	if os.Getenv("APP_ENV") != "production" {
		if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Fatalf("gagal membaca .env: %v", err)
		}
	}
	url := os.Getenv("PLATFORM_DATABASE_URL")
	if url == "" {
		log.Fatal("PLATFORM_DATABASE_URL belum diset")
	}
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		log.Fatal("PLATFORM_DATABASE_URL tidak valid")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), telemetry.Install(poolCfg))
	if err != nil {
		log.Fatal("PLATFORM_DATABASE_URL tidak valid")
	}
	defer pool.Close()
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("tidak bisa terhubung ke database platform")
	}
	encKey, err := dbconn.ParseKey(os.Getenv("ENCRYPTION_KEY"))
	if err != nil {
		log.Fatalf("worker membutuhkan ENCRYPTION_KEY yang sama dengan API: %v", err)
	}
	conns := &dbconn.Registry{DB: pool, Key: encKey}
	defer conns.Close()

	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		os.Setenv("OTEL_SERVICE_NAME", "if3110-worker")
	}
	observer, err := observability.New(context.Background(), pool, conns)
	if err != nil {
		log.Fatal("observability tidak dapat diinisialisasi")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		observer.Shutdown(ctx)
	}()
	log.SetOutput(slog.NewLogLogger(slog.Default().Handler(), slog.LevelInfo).Writer())
	// Setelah logger JSON siap, supaya SQL migrasi pun tercatat sebagai JSON. Aman dijalankan bersamaan
	// dengan API: migrasi platform dilindungi advisory lock.
	if err := platformdb.Migrate(context.Background(), pool); err != nil {
		log.Fatalf("migrasi platform DB gagal: %v", err)
	}
	metrics, err := telemetry.NewMetrics(pool, observer.Registry)
	if err != nil {
		log.Fatalf("metrics tidak dapat didaftarkan: %v", err)
	}

	id := os.Getenv("WORKER_ID")
	if id == "" {
		host, _ := os.Hostname()
		id = host + "-" + strconv.Itoa(os.Getpid())
	}
	w := &worker.Worker{
		DB: pool, Flow: &workflow.Service{DB: pool, Authz: &authz.Authorizer{DB: pool}}, Targets: conns, ID: id,
		Lease:            duration("WORKER_LEASE", 30*time.Second),
		Poll:             duration("WORKER_POLL", time.Second),
		LockWait:         duration("WORKER_LOCK_WAIT", 30*time.Second),
		StatementTimeout: duration("WORKER_SQL_TIMEOUT", 30*time.Minute), // terpisah dari timeout HTTP
		Metrics:          metrics,
	}

	addr := os.Getenv("WORKER_METRICS_ADDR")
	if addr == "" {
		addr = ":9102"
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", observer.Metrics())
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("ok\n")) })
	metricsServer := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server metrics berhenti: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("worker %s berjalan; metrics di %s", id, addr)
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("worker berhenti: %v", err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	metricsServer.Shutdown(shutdown)
}
