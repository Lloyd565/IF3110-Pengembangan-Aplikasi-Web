package testdb

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// FreshMaria uses a dedicated test instance; only the it_* schema is recreated.
func FreshMaria(t testing.TB, name string) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if dsn == "" {
		t.Skip("TEST_MARIADB_DSN belum diset")
	}
	if !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(name) {
		t.Fatal("nama database test tidak valid")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("TEST_MARIADB_DSN tidak valid")
	}
	cfg.DBName = ""
	cfg.MultiStatements = false
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	defer admin.Close()
	ctx := context.Background()
	database := "it_" + name
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+database+"`"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+database+"` CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	cfg.DBName = database
	cfg.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'", "default_storage_engine": "'InnoDB'"}
	connector, err = mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	return db, database
}
