package dbconn

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/go-sql-driver/mysql"
)

func TestDBMSAndMariaConfig(t *testing.T) {
	for _, value := range []DBMS{"", PostgreSQL, MariaDB} {
		if _, err := NormalizeDBMS(value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NormalizeDBMS("oracle"); err == nil {
		t.Fatal("unsupported DBMS accepted")
	}
	for _, mode := range []string{"disable", "require", "verify-full"} {
		p := Params{Host: "::1", Port: 3306, DBName: "target", Username: "u", Password: "p@ss:/?#", SSLMode: mode, DBMS: MariaDB}
		cfg, err := mariaConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MultiStatements || cfg.Passwd != p.Password || cfg.Addr != "[::1]:3306" {
			t.Fatal("unsafe or corrupt MariaDB config")
		}
		parsed, err := mysql.ParseDSN(cfg.FormatDSN())
		if err != nil || parsed.Passwd != p.Password {
			t.Fatal("DSN roundtrip", err)
		}
	}
	if _, err := mariaConfig(Params{SSLMode: "prefer"}); err == nil {
		t.Fatal("ambiguous TLS mode accepted")
	}
	for _, v := range []string{"10.6.1-MariaDB", "11.4.8-MariaDB", "12.2.2-MariaDB", "5.5.5-10.11.8-MariaDB"} {
		if !supportedMariaVersion(v) {
			t.Fatalf("version rejected %s", v)
		}
	}
	for _, v := range []string{"10.6.0-MariaDB", "10.5.30-MariaDB", "8.4.0", "garbage"} {
		if supportedMariaVersion(v) {
			t.Fatalf("unsupported version accepted %s", v)
		}
	}
}

func TestIntegrationMariaRegistry(t *testing.T) {
	db, database := testdb.FreshMaria(t, "maria_registry")
	_ = db
	platform := testdb.Fresh(t, "maria_registry_platform")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(os.Getenv("TEST_MARIADB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	p := Params{Host: host, Port: port, DBName: database, Username: cfg.User, Password: cfg.Passwd, DBMS: MariaDB, SSLMode: "disable"}
	reg := &Registry{DB: platform, Key: randKey(t)}
	defer reg.Close()
	id := insertConn(t, platform, reg, "MariaDB", p)
	if _, err := platform.Exec(ctx, `UPDATE db_connections SET dbms='mariadb' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	a, err := reg.Target(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.Target(ctx, id)
	if err != nil || a != b || a.DBMS != MariaDB {
		t.Fatal("target cache", err)
	}
	if _, err := reg.Pool(ctx, id); err == nil {
		t.Fatal("MariaDB exposed as pgx pool")
	}
	if err := Check(ctx, p); err != nil {
		t.Fatal(err)
	}
	reg.Invalidate(id)
	c, err := reg.Target(ctx, id)
	if err != nil || c == a {
		t.Fatal("invalidation", err)
	}
	p.Port = 1
	p.Password = "must-not-appear"
	if _, err := ConnectTarget(ctx, p); err == nil || strings.Contains(err.Error(), p.Password) {
		t.Fatal("connection failure leaked password", err)
	}
}
