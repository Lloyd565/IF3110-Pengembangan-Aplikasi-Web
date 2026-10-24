package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/httpx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
)

func main() {
	dbURL := os.Getenv("PLATFORM_DATABASE_URL")
	if dbURL == "" {
		log.Fatal("PLATFORM_DATABASE_URL belum diset")
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		log.Fatalf("URL database tidak valid: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("tidak bisa terhubung ke database: %v", err)
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

	srv := &httpx.Server{
		DB:       pool,
		Sessions: &auth.Sessions{DB: pool},
		Authz:    &authz.Authorizer{DB: pool},
		Conns:    &dbconn.Registry{DB: pool},
	}

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", srv.Routes()))
}
