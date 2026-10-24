package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/config"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/httpx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/user"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatal("PLATFORM_DATABASE_URL tidak valid")
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		log.Fatal("tidak bisa terhubung ke database platform")
	}
	log.Println("terhubung ke platform DB")

	if err := platformdb.Migrate(context.Background(), pool); err != nil {
		log.Fatalf("migrasi platform DB gagal: %v", err)
	}
	if n, err := migration.RecoverInterrupted(context.Background(), pool); err != nil {
		log.Fatalf("pemulihan migration yang terputus gagal: %v", err)
	} else if n > 0 {
		log.Printf("PERINGATAN: %d migration yang terputus ditandai failed; periksa Target DB secara manual", n)
	}

	// Kunci enkripsi password koneksi database target. Wajib di production;
	// di development hanya peringatan (login tetap jalan, tapi koneksi tidak bisa dibuat/dibuka).
	encKey, err := dbconn.ParseKey(os.Getenv("ENCRYPTION_KEY"))
	if err != nil {
		if cfg.Production {
			log.Fatal(err)
		}
		log.Printf("PERINGATAN: %v; koneksi database target tidak bisa dibuat atau dibuka", err)
	}

	conns := &dbconn.Registry{DB: pool, Key: encKey}
	defer conns.Close()
	srv := &httpx.Server{
		PublicOrigin: cfg.PublicOrigin,
		DB:           pool,
		Sessions:     &auth.Sessions{DB: pool},
		Authz:        &authz.Authorizer{DB: pool},
		Conns:        conns,
	}
	srv.Admin = &admin.Service{
		DB: pool, Users: &user.Repo{DB: pool}, Sessions: srv.Sessions, Conns: conns, Hash: auth.HashPassword,
	}
	bootUser := os.Getenv("BOOTSTRAP_ADMIN_USERNAME")
	if created, err := srv.Admin.Bootstrap(context.Background(), bootUser, os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")); err != nil {
		log.Fatalf("bootstrap admin pertama gagal: %v", err)
	} else if created {
		log.Printf("admin pertama dibuat: %s", bootUser)
	}
	if err := srv.ConfigureAuth(cfg.SessionPolicy, cfg.SessionKey, cfg.Production); err != nil {
		log.Fatalf("konfigurasi autentikasi gagal: %v", err)
	}

	server := &http.Server{Addr: cfg.Address, Handler: srv.Routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 1 << 20}
	ctxSignal, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctxSignal.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			server.Close()
		}
	}()
	log.Printf("listening on %s", cfg.Address)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server berhenti: %v", err)
		stop()
	}
	<-done
}
