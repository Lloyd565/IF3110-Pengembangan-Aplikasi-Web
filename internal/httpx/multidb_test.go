package httpx_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/httpx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/user"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestIntegrationBlackboxMultipleDBMS(t *testing.T) {
	ctx := context.Background()
	maria, mariaName := testdb.FreshMaria(t, "multidb_http")
	pg := testdb.Fresh(t, "multidb_pg_target")
	platform := testdb.Fresh(t, "multidb_http_platform")
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(ctx, "CREATE TABLE items(id INT PRIMARY KEY,title VARCHAR(100)); INSERT INTO items VALUES(1,'seed')"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE items(id INT PRIMARY KEY,title VARCHAR(100))", "INSERT INTO items VALUES(1,'seed')",
		"CREATE USER IF NOT EXISTS 'if3110_target'@'%' IDENTIFIED BY 'fixture-password'",
		"GRANT ALL ON `" + mariaName + "`.* TO 'if3110_target'@'%'",
	} {
		if _, err := maria.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	reg := &dbconn.Registry{DB: platform, Key: []byte(strings.Repeat("e", 32))}
	defer reg.Close()
	sessions := &auth.Sessions{DB: platform}
	users := &user.Repo{DB: platform}
	svc := &admin.Service{DB: platform, Users: users, Sessions: sessions, Conns: reg, Hash: auth.HashPassword}
	hash, err := auth.HashPassword("blackbox-secret-password")
	if err != nil {
		t.Fatal(err)
	}
	root, err := users.Create(ctx, "Budi", hash, true)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := users.Create(ctx, "Reader", hash, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(ctx, "Outsider", hash, false); err != nil {
		t.Fatal(err)
	}
	editGroup, err := svc.CreateGroup(ctx, root, "editors", "")
	if err != nil {
		t.Fatal(err)
	}
	viewGroup, err := svc.CreateGroup(ctx, root, "readers", "")
	if err != nil {
		t.Fatal(err)
	}
	pgCfg := pg.Config().ConnConfig
	password := pgCfg.Password
	if password == "" {
		password = "unused-on-trust-fixture"
	}
	pgID, err := svc.CreateConnection(ctx, root, "PostgreSQL", dbconn.Params{Host: pgCfg.Host, Port: int(pgCfg.Port), DBName: pgCfg.Database, Username: pgCfg.User, Password: password, SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	mariaCfg, err := mysql.ParseDSN(os.Getenv("TEST_MARIADB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	mariaHost, mariaPort, err := net.SplitHostPort(mariaCfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(mariaPort)
	mariaParams := dbconn.Params{DBMS: dbconn.MariaDB, Host: mariaHost, Port: port, DBName: mariaName, Username: "if3110_target", Password: "fixture-password", SSLMode: "disable"}
	mariaID, err := svc.CreateConnection(ctx, root, "MariaDB", mariaParams)
	if err != nil {
		t.Fatal(err)
	}
	s := &httpx.Server{DB: platform, Sessions: sessions, Authz: &authz.Authorizer{DB: platform}, Conns: reg, Admin: svc}
	if err := s.ConfigureAuth(auth.DefaultPolicy(), []byte(strings.Repeat("s", 32)), true); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(s.Routes())
	defer server.Close()
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a := &blackboxApp{db: platform, server: s, url: server.URL, client: client}
	login := func(username string) string {
		t.Helper()
		a.client.Jar, _ = cookiejar.New(nil)
		_, body := a.request(t, "GET", "/login", nil)
		r, _ := a.request(t, "POST", "/login", url.Values{"username": {username}, "password": {"blackbox-secret-password"}, "csrf_token": {csrfFromBody(t, body)}})
		if r.StatusCode != 303 {
			t.Fatalf("login %s status %d", username, r.StatusCode)
		}
		r, body = a.request(t, "GET", "/dashboard", nil)
		if r.StatusCode != 200 {
			t.Fatal("dashboard")
		}
		return csrfFromBody(t, body)
	}
	csrf := login("Budi")
	for _, id := range []uuid.UUID{pgID, mariaID} {
		r, _ := a.request(t, "GET", fmt.Sprintf("/db/%s/schema", id), nil)
		if r.StatusCode != 403 {
			t.Fatal("admin bypassed database grant")
		}
	}
	for _, id := range []uuid.UUID{pgID, mariaID} {
		if err := svc.SetGrant(ctx, root, editGroup, id, "edit"); err != nil {
			t.Fatal(err)
		}
		if err := svc.SetGrant(ctx, root, viewGroup, id, "view"); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AddMember(ctx, root, editGroup, root); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(ctx, root, viewGroup, reader); err != nil {
		t.Fatal(err)
	}
	t.Run("admin form preserves DBMS and tests connection", func(t *testing.T) {
		r, body := a.request(t, "GET", "/admin/connections?edit="+mariaID.String(), nil)
		if r.StatusCode != 200 || !strings.Contains(body, `value="mariadb" selected`) {
			t.Fatal("DBMS selector missing")
		}
		form := url.Values{"csrf_token": {csrf}, "action": {"test"}, "dbms": {"mariadb"}, "name": {"MariaDB"}, "host": {mariaParams.Host}, "port": {strconv.Itoa(port)}, "dbname": {mariaName}, "username": {mariaParams.Username}, "password": {mariaParams.Password}, "sslmode": {"disable"}}
		r, body = a.request(t, "POST", "/admin/connections", form)
		if r.StatusCode != 200 || !strings.Contains(body, "Koneksi berhasil.") || strings.Contains(body, mariaParams.Password) {
			t.Fatal("test connection/password leak")
		}
		form.Set("action", "create")
		form.Set("dbms", "unknown")
		r, _ = a.request(t, "POST", "/admin/connections", form)
		if r.StatusCode != 422 {
			t.Fatal("unsupported DBMS accepted")
		}
	})
	var pgMigrationPath string
	for _, tc := range []struct {
		name, schema string
		id           uuid.UUID
	}{{"postgresql", "public", pgID}, {"mariadb", mariaName, mariaID}} {
		t.Run(tc.name, func(t *testing.T) {
			base := "/db/" + tc.id.String()
			for _, path := range []string{base + "/schema", base + "/tables/" + tc.schema + "/items", base + "/data/" + tc.schema + "/items"} {
				r, body := a.request(t, "GET", path, nil)
				if r.StatusCode != 200 || !strings.Contains(body, "items") {
					t.Fatalf("explorer %s returned %d", path, r.StatusCode)
				}
			}
			create := func(q string) string {
				t.Helper()
				r, _ := a.request(t, "POST", base+"/migrations/new", url.Values{"csrf_token": {csrf}, "name": {"HTTP migration"}, "sql": {q}})
				if r.StatusCode != 303 {
					t.Fatalf("create returned %d", r.StatusCode)
				}
				return r.Header.Get("Location")
			}
			path := create("ALTER TABLE items ADD COLUMN flag INT DEFAULT 0")
			if tc.name == "postgresql" {
				pgMigrationPath = path
			}
			r, _ := a.request(t, "POST", path+"/execute", url.Values{"csrf_token": {csrf}})
			if r.StatusCode != 303 {
				t.Fatalf("execute returned %d", r.StatusCode)
			}
			r, body := a.request(t, "GET", path, nil)
			if r.StatusCode != 200 || !strings.Contains(body, ">success</span>") {
				t.Fatal("success history")
			}
			path = create("INSERT INTO items VALUES(2,'must-rollback',0); INSERT INTO items VALUES(1,'duplicate',0)")
			r, _ = a.request(t, "POST", path+"/execute", url.Values{"csrf_token": {csrf}})
			if r.StatusCode != 303 {
				t.Fatal("failed SQL did not produce history")
			}
			r, body = a.request(t, "GET", path, nil)
			if r.StatusCode != 200 || !strings.Contains(body, ">failed</span>") {
				t.Fatal("failure history")
			}
			r, body = a.request(t, "GET", base+"/data/"+tc.schema+"/items", nil)
			if r.StatusCode != 200 || strings.Contains(body, "must-rollback") || !strings.Contains(body, "flag") {
				t.Fatal("commit/rollback effects incorrect")
			}
			if tc.name == "mariadb" {
				r, _ := a.request(t, "POST", base+"/migrations/new", url.Values{"csrf_token": {csrf}, "name": {"unsafe"}, "sql": {"CREATE TABLE partial(id INT); INSERT INTO partial VALUES(1)"}})
				if r.StatusCode != 422 {
					t.Fatal("unsafe DDL batch accepted")
				}
			}
		})
	}
	t.Run("migration ID cannot cross connections", func(t *testing.T) {
		parts := strings.Split(pgMigrationPath, "/")
		r, _ := a.request(t, "GET", "/db/"+mariaID.String()+"/migrations/"+parts[len(parts)-1], nil)
		if r.StatusCode != 404 {
			t.Fatal("cross-connection migration access")
		}
	})
	csrf = login("Reader")
	for _, tc := range []struct {
		id     uuid.UUID
		schema string
	}{{pgID, "public"}, {mariaID, mariaName}} {
		base := "/db/" + tc.id.String()
		r, _ := a.request(t, "GET", base+"/data/"+tc.schema+"/items", nil)
		if r.StatusCode != 200 {
			t.Fatal("view user cannot read")
		}
		r, _ = a.request(t, "POST", base+"/migrations/new", url.Values{"csrf_token": {csrf}, "name": {"forbidden"}, "sql": {"DROP TABLE items"}})
		if r.StatusCode != 403 {
			t.Fatal("view user can mutate")
		}
	}
	login("Outsider")
	for _, id := range []uuid.UUID{pgID, mariaID} {
		r, _ := a.request(t, "GET", "/db/"+id.String()+"/schema", nil)
		if r.StatusCode != 403 {
			t.Fatal("ungranted user can read")
		}
	}
}
