package httpx_test

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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
	"github.com/jackc/pgx/v5/pgxpool"
)

type diffApp struct {
	*blackboxApp
	source, reference              *dbconn.Target
	sourceID, referenceID, crossID uuid.UUID
	sourceSchema, referenceSchema  string
}

func postgresParams(pool *pgxpool.Pool) dbconn.Params {
	cfg := pool.Config().ConnConfig
	password := cfg.Password
	if password == "" {
		password = "unused-trust-fixture"
	}
	return dbconn.Params{DBMS: dbconn.PostgreSQL, Host: cfg.Host, Port: int(cfg.Port), DBName: cfg.Database, Username: cfg.User, Password: password, SSLMode: "disable"}
}

func newDiffApp(t *testing.T, kind dbconn.DBMS) *diffApp {
	t.Helper()
	ctx := context.Background()
	a := &diffApp{}
	var sourceParams, referenceParams dbconn.Params
	if kind == dbconn.PostgreSQL {
		src := testdb.Fresh(t, "diff_http_pg_current")
		ref := testdb.Fresh(t, "diff_http_pg_reference")
		a.source = &dbconn.Target{DBMS: kind, Database: src.Config().ConnConfig.Database, Postgres: src}
		a.reference = &dbconn.Target{DBMS: kind, Database: ref.Config().ConnConfig.Database, Postgres: ref}
		a.sourceSchema, a.referenceSchema = "public", "public"
		sourceParams, referenceParams = postgresParams(src), postgresParams(ref)
	} else {
		src, srcName := testdb.FreshMaria(t, "diff_http_maria_current")
		ref, refName := testdb.FreshMaria(t, "diff_http_maria_reference")
		a.source = &dbconn.Target{DBMS: kind, Database: srcName, Maria: src}
		a.reference = &dbconn.Target{DBMS: kind, Database: refName, Maria: ref}
		a.sourceSchema, a.referenceSchema = srcName, refName
		for _, q := range []string{"CREATE USER IF NOT EXISTS 'diff_target'@'%' IDENTIFIED BY 'fixture-password'", "GRANT ALL ON `" + srcName + "`.* TO 'diff_target'@'%'", "GRANT ALL ON `" + refName + "`.* TO 'diff_target'@'%'"} {
			if _, err := src.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
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
		sourceParams = dbconn.Params{DBMS: kind, Host: host, Port: port, DBName: srcName, Username: "diff_target", Password: "fixture-password", SSLMode: "disable"}
		referenceParams = sourceParams
		referenceParams.DBName = refName
	}
	if err := a.exec(false, "CREATE TABLE items(id INT PRIMARY KEY,title VARCHAR(20),obsolete TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := a.exec(false, "INSERT INTO items VALUES(1,'preserve','legacy')"); err != nil {
		t.Fatal(err)
	}
	if err := a.exec(true, "CREATE TABLE items(id INT PRIMARY KEY,title VARCHAR(80),flag INT NOT NULL DEFAULT 0)"); err != nil {
		t.Fatal(err)
	}
	if err := a.exec(true, "CREATE TABLE added(id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	platform := testdb.Fresh(t, "diff_http_platform_"+string(kind))
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	reg := &dbconn.Registry{DB: platform, Key: []byte(strings.Repeat("e", 32))}
	sessions := &auth.Sessions{DB: platform}
	users := &user.Repo{DB: platform}
	svc := &admin.Service{DB: platform, Users: users, Sessions: sessions, Conns: reg, Hash: auth.HashPassword}
	hash, err := auth.HashPassword("blackbox-secret-password")
	if err != nil {
		t.Fatal(err)
	}
	root, err := users.Create(ctx, "Root", hash, true)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]int64{}
	for _, name := range []string{"Editor", "Reader", "SourceOnly", "Outsider"} {
		id, err := users.Create(ctx, name, hash, false)
		if err != nil {
			t.Fatal(err)
		}
		group, err := svc.CreateGroup(ctx, root, name, "")
		if err != nil {
			t.Fatal(err)
		}
		groups[name] = group
		if err := svc.AddMember(ctx, root, group, id); err != nil {
			t.Fatal(err)
		}
	}
	a.sourceID, err = svc.CreateConnection(ctx, root, "Source", sourceParams)
	if err != nil {
		t.Fatal(err)
	}
	a.referenceID, err = svc.CreateConnection(ctx, root, "Reference", referenceParams)
	if err != nil {
		t.Fatal(err)
	}
	otherKind := dbconn.MariaDB
	if kind == dbconn.MariaDB {
		otherKind = dbconn.PostgreSQL
	}
	a.crossID, err = svc.CreateConnection(ctx, root, "Cross DBMS", dbconn.Params{DBMS: otherKind, Host: "127.0.0.1", Port: 1, DBName: "unused", Username: "u", Password: "fixture", SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	for name, group := range groups {
		if name == "Outsider" {
			continue
		}
		level := "view"
		if name != "Reader" {
			level = "edit"
		}
		if err := svc.SetGrant(ctx, root, group, a.sourceID, level); err != nil {
			t.Fatal(err)
		}
		if name != "SourceOnly" {
			if err := svc.SetGrant(ctx, root, group, a.referenceID, "view"); err != nil {
				t.Fatal(err)
			}
		}
		if name == "Editor" {
			if err := svc.SetGrant(ctx, root, group, a.crossID, "view"); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := &httpx.Server{DB: platform, Conns: reg, Sessions: sessions, Authz: &authz.Authorizer{DB: platform}, Admin: svc}
	if err := s.ConfigureAuth(auth.DefaultPolicy(), []byte(strings.Repeat("s", 32)), true); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(s.Routes())
	t.Cleanup(func() { server.Close(); reg.Close() })
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a.blackboxApp = &blackboxApp{db: platform, server: s, url: server.URL, client: client}
	return a
}

func (a *diffApp) exec(reference bool, q string) error {
	target := a.source
	if reference {
		target = a.reference
	}
	if target.DBMS == dbconn.PostgreSQL {
		_, err := target.Postgres.Exec(context.Background(), q)
		return err
	}
	_, err := target.Maria.ExecContext(context.Background(), q)
	return err
}
func (a *diffApp) loginAs(t *testing.T, name string) string {
	t.Helper()
	a.client.Jar, _ = cookiejar.New(nil)
	_, body := a.request(t, "GET", "/login", nil)
	r, _ := a.request(t, "POST", "/login", url.Values{"username": {name}, "password": {"blackbox-secret-password"}, "csrf_token": {csrfFromBody(t, body)}})
	if r.StatusCode != 303 {
		t.Fatalf("login %s: %d", name, r.StatusCode)
	}
	_, body = a.request(t, "GET", "/dashboard", nil)
	return csrfFromBody(t, body)
}
func diffHash(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`data-` + name + `-hash="([0-9a-f]{64})"`).FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("%s fingerprint missing from preview", name)
	}
	return m[1]
}

func TestIntegrationBlackboxSchemaDiff(t *testing.T) {
	for _, kind := range []dbconn.DBMS{dbconn.PostgreSQL, dbconn.MariaDB} {
		t.Run(string(kind), func(t *testing.T) {
			a := newDiffApp(t, kind)
			base := "/db/" + a.sourceID.String() + "/schema/diff"
			r, _ := a.request(t, "GET", base, nil)
			if r.StatusCode != 303 {
				t.Fatal("anonymous schema diff access")
			}
			csrf := a.loginAs(t, "Editor")
			form := func() url.Values {
				return url.Values{"csrf_token": {csrf}, "reference_id": {a.referenceID.String()}, "source_schema": {a.sourceSchema}, "reference_schema": {a.referenceSchema}, "name": {"Generated diff"}}
			}
			r, body := a.request(t, "GET", base+"?reference_id="+a.referenceID.String(), nil)
			if r.StatusCode != 200 || !strings.Contains(body, "Schema diff") {
				t.Fatalf("form %d: %s", r.StatusCode, body)
			}
			missing := form()
			missing.Del("csrf_token")
			r, _ = a.request(t, "POST", base, missing)
			if r.StatusCode != 403 {
				t.Fatal("CSRF preview bypass")
			}
			r, _ = a.request(t, "POST", base+"/draft", missing)
			if r.StatusCode != 403 {
				t.Fatal("CSRF draft bypass")
			}
			invalidSchema := form()
			invalidSchema.Set("source_schema", `public";DROP TABLE items;--`)
			r, _ = a.request(t, "POST", base, invalidSchema)
			if r.StatusCode != 422 {
				t.Fatal("invalid schema accepted")
			}
			cross := form()
			cross.Set("reference_id", a.crossID.String())
			r, _ = a.request(t, "POST", base, cross)
			if r.StatusCode != 422 {
				t.Fatal("cross DBMS accepted")
			}
			r, body = a.request(t, "POST", base, form())
			if r.StatusCode != 200 || !strings.Contains(html.UnescapeString(body), "ALTER TABLE") {
				t.Fatalf("preview %d: %s", r.StatusCode, body)
			}
			var count int
			if err := a.db.QueryRow(context.Background(), "SELECT COUNT(*) FROM migrations").Scan(&count); err != nil || count != 0 {
				t.Fatal("preview wrote migrations", err)
			}
			preview := body
			save := form()
			save.Set("source_hash", diffHash(t, body, "source"))
			save.Set("reference_hash", diffHash(t, body, "reference"))
			r, _ = a.request(t, "POST", base+"/draft", save)
			if r.StatusCode != 422 {
				t.Fatal("destructive change accepted without acknowledgment")
			}
			save.Set("ack_destructive", "1")
			if err := a.exec(false, "ALTER TABLE items ADD COLUMN transient_col INT"); err != nil {
				t.Fatal(err)
			}
			r, _ = a.request(t, "POST", base+"/draft", save)
			if r.StatusCode != 409 {
				t.Fatal("stale active schema accepted")
			}
			if err := a.exec(false, "ALTER TABLE items DROP COLUMN transient_col"); err != nil {
				t.Fatal(err)
			}
			if err := a.exec(true, "ALTER TABLE items ADD COLUMN late INT"); err != nil {
				t.Fatal(err)
			}
			r, _ = a.request(t, "POST", base+"/draft", save)
			if r.StatusCode != 409 {
				t.Fatal("stale reference schema accepted")
			}
			r, body = a.request(t, "POST", base, form())
			if r.StatusCode != 200 {
				t.Fatal("refresh preview")
			}
			save.Set("source_hash", diffHash(t, body, "source"))
			save.Set("reference_hash", diffHash(t, body, "reference"))
			save.Set("sql", "DROP TABLE must_not_be_used")
			// Generation must not execute SQL; applying drift is reserved for execution.
			if !strings.Contains(html.UnescapeString(preview), "obsolete") {
				t.Fatal("removed column not shown")
			}
			r, _ = a.request(t, "POST", base+"/draft", save)
			if r.StatusCode != 303 {
				t.Fatalf("save drafts %d", r.StatusCode)
			}
			location := r.Header.Get("Location")
			var ids []int64
			if kind == dbconn.PostgreSQL {
				parts := strings.Split(location, "/")
				id, err := strconv.ParseInt(parts[len(parts)-2], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			} else {
				u, err := url.Parse(location)
				if err != nil {
					t.Fatal(err)
				}
				for _, part := range strings.Split(u.Query().Get("generated"), ",") {
					id, err := strconv.ParseInt(part, 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
				r, body = a.request(t, "GET", location, nil)
				if r.StatusCode != 200 || !strings.Contains(body, "urutan pengerjaan") {
					t.Fatal("ordered MariaDB draft overview missing")
				}
			}
			if len(ids) == 0 {
				t.Fatal("no generated drafts")
			}
			r, body = a.request(t, "GET", "/db/"+a.sourceID.String()+"/data/"+a.sourceSchema+"/items", nil)
			if r.StatusCode != 200 || !strings.Contains(body, "obsolete") {
				t.Fatal("saving draft executed target SQL")
			}
			for _, id := range ids {
				var status, q string
				if err := a.db.QueryRow(context.Background(), "SELECT status,sql FROM migrations WHERE id=$1", id).Scan(&status, &q); err != nil || status != "draft" || strings.Contains(q, "must_not_be_used") {
					t.Fatal("generation bypassed draft or accepted client SQL", err)
				}
				path := fmt.Sprintf("/db/%s/migrations/%d", a.sourceID, id)
				r, body = a.request(t, "GET", path+"/edit", nil)
				if r.StatusCode != 200 || !strings.Contains(body, "textarea") {
					t.Fatal("generated draft not editable")
				}
				r, _ = a.request(t, "POST", path+"/edit", url.Values{"csrf_token": {csrf}, "name": {"Reviewed"}, "sql": {q + "\n-- reviewed\n"}})
				if r.StatusCode != 303 {
					t.Fatal("cannot edit generated SQL")
				}
				r, _ = a.request(t, "POST", path+"/execute", url.Values{"csrf_token": {csrf}})
				if r.StatusCode != 303 {
					t.Fatal("execute generated SQL")
				}
				r, body = a.request(t, "GET", path, nil)
				if r.StatusCode != 200 || !strings.Contains(body, ">success</span>") {
					t.Fatalf("generated migration failed: %s", body)
				}
			}
			r, body = a.request(t, "POST", base, form())
			if r.StatusCode != 200 || !strings.Contains(body, "Tidak ada perbedaan") {
				t.Fatalf("roundtrip failed: %d %s", r.StatusCode, body)
			}
			noop := form()
			noop.Set("source_hash", diffHash(t, body, "source"))
			noop.Set("reference_hash", diffHash(t, body, "reference"))
			r, _ = a.request(t, "POST", base+"/draft", noop)
			if r.StatusCode != 422 {
				t.Fatal("no-op generated a migration")
			}
			if err := a.exec(true, "CREATE VIEW manual_view AS SELECT id FROM items"); err != nil {
				t.Fatal(err)
			}
			r, body = a.request(t, "POST", base, form())
			if r.StatusCode != 200 || !strings.Contains(body, "Penanganan manual diperlukan") {
				t.Fatal("unsupported view silently ignored")
			}
			manual := form()
			manual.Set("source_hash", diffHash(t, body, "source"))
			manual.Set("reference_hash", diffHash(t, body, "reference"))
			r, _ = a.request(t, "POST", base+"/draft", manual)
			if r.StatusCode != 422 {
				t.Fatal("unsupported metadata generated drafts")
			}
			if err := a.exec(true, "DROP VIEW manual_view"); err != nil {
				t.Fatal(err)
			}
			csrf = a.loginAs(t, "Reader")
			r, body = a.request(t, "POST", base, form())
			if r.StatusCode != 200 {
				t.Fatal("view user cannot compare")
			}
			readerSave := form()
			readerSave.Set("source_hash", diffHash(t, body, "source"))
			readerSave.Set("reference_hash", diffHash(t, body, "reference"))
			readerSave.Set("ack_destructive", "1")
			r, _ = a.request(t, "POST", base+"/draft", readerSave)
			if r.StatusCode != 403 {
				t.Fatal("view user can generate persisted drafts")
			}
			csrf = a.loginAs(t, "SourceOnly")
			r, _ = a.request(t, "POST", base, form())
			if r.StatusCode != 403 {
				t.Fatal("reference grant not checked")
			}
			csrf = a.loginAs(t, "Outsider")
			r, _ = a.request(t, "POST", base, form())
			if r.StatusCode != 403 {
				t.Fatal("source grant not checked")
			}
			csrf = a.loginAs(t, "Root")
			r, _ = a.request(t, "POST", base, form())
			if r.StatusCode != 403 {
				t.Fatal("admin bypassed database grants")
			}
		})
	}
}
