package httpx

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/user"
)

// Handler dipanggil langsung (bukan lewat Routes) dengan Principal di context,
// sehingga test ini hanya menguji lapisan akses + halaman admin.

type admEnv struct {
	srv  *Server
	db   *pgxpool.Pool
	svc  *admin.Service
	root int64
}

func admSetup(t *testing.T) *admEnv {
	t.Helper()
	ctx := context.Background()
	db := testdb.Fresh(t, "httpx_admin")
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	reg := &dbconn.Registry{DB: db, Key: key}
	users := &user.Repo{DB: db}
	svc := &admin.Service{
		DB: db, Users: users, Sessions: &auth.Sessions{DB: db}, Conns: reg,
		Hash: func(pw string) (string, error) { return "hash:" + pw, nil },
	}
	root, err := users.Create(ctx, "root", "h", true)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{DB: db, Authz: &authz.Authorizer{DB: db}, Conns: reg, Admin: svc, Sessions: &auth.Sessions{DB: db}}
	return &admEnv{srv: srv, db: db, svc: svc, root: root}
}

func admPrincipal(id int64, isAdmin bool) *reqctx.Principal {
	return &reqctx.Principal{UserID: id, Username: fmt.Sprintf("u%d", id), IsPlatformAdmin: isAdmin}
}

func (e *admEnv) call(h http.HandlerFunc, method, target string, form url.Values, p *reqctx.Principal, pathID string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if p != nil {
		req = req.WithContext(reqctx.WithPrincipal(req.Context(), *p))
	}
	if pathID != "" {
		req.SetPathValue("id", pathID)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func (e *admEnv) asAdmin(h http.HandlerFunc, method, target string, form url.Values) *httptest.ResponseRecorder {
	return e.call(h, method, target, form, admPrincipal(e.root, true), "")
}

func (e *admEnv) mkUser(t *testing.T, name string) int64 {
	t.Helper()
	id, err := e.svc.CreateUser(context.Background(), e.root, name, "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *admEnv) mkSession(t *testing.T, userID int64) {
	t.Helper()
	if _, err := e.db.Exec(context.Background(), `
		INSERT INTO sessions (user_id, token_hash, csrf_token, absolute_expires_at)
		VALUES ($1, $2, 'c', now() + interval '8 hours')`, userID, []byte(uuid.NewString())); err != nil {
		t.Fatal(err)
	}
}

func (e *admEnv) forceRotate(t *testing.T, userID int64) bool {
	t.Helper()
	var v bool
	if err := e.db.QueryRow(context.Background(), `SELECT force_rotate FROM sessions WHERE user_id = $1`, userID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func admBody(rr *httptest.ResponseRecorder) string { return rr.Body.String() }

func TestAdminPagesAccess(t *testing.T) {
	e := admSetup(t)
	user1 := e.mkUser(t, "biasa")

	pages := map[string]http.HandlerFunc{
		"/admin/users": e.srv.adminUsers, "/admin/groups": e.srv.adminGroups,
		"/admin/connections": e.srv.adminConnections, "/admin/grants": e.srv.adminGrants,
	}
	for path, h := range pages {
		if rr := e.call(h, "GET", path, nil, nil, ""); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
			t.Errorf("%s tanpa login: %d %s", path, rr.Code, rr.Header().Get("Location"))
		}
		if rr := e.call(h, "GET", path, nil, admPrincipal(user1, false), ""); rr.Code != http.StatusForbidden {
			t.Errorf("%s user biasa harus 403, dapat %d", path, rr.Code)
		}
		if rr := e.call(h, "POST", path, url.Values{"action": {"create"}}, admPrincipal(user1, false), ""); rr.Code != http.StatusForbidden {
			t.Errorf("POST %s user biasa harus 403, dapat %d", path, rr.Code)
		}
		if rr := e.asAdmin(h, "GET", path, nil); rr.Code != http.StatusOK {
			t.Errorf("%s admin harus 200, dapat %d: %s", path, rr.Code, admBody(rr))
		}
		if rr := e.asAdmin(h, "DELETE", path, nil); rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s DELETE harus 405, dapat %d", path, rr.Code)
		}
	}
}

func TestAdminUsersPage(t *testing.T) {
	e := admSetup(t)
	h := e.srv.adminUsers

	rr := e.asAdmin(h, "POST", "/admin/users", url.Values{"action": {"create"}, "username": {"budi"}, "password": {"password123"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users?ok=created" {
		t.Fatalf("buat user: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	if body := admBody(e.asAdmin(h, "GET", "/admin/users?ok=created", nil)); !strings.Contains(body, "budi") || !strings.Contains(body, "Berhasil dibuat.") {
		t.Errorf("daftar user: %s", body)
	}
	if body := admBody(e.asAdmin(h, "GET", "/admin/users?ok=<script>", nil)); strings.Contains(body, "<script>") {
		t.Error("pesan sukses tidak boleh mengambil teks bebas dari URL")
	}

	dup := e.asAdmin(h, "POST", "/admin/users", url.Values{"action": {"create"}, "username": {"BUDI"}, "password": {"password123"}})
	if dup.Code != http.StatusUnprocessableEntity || !strings.Contains(admBody(dup), "sudah dipakai") {
		t.Errorf("username kembar: %d", dup.Code)
	}
	if rr := e.asAdmin(h, "POST", "/admin/users", url.Values{"action": {"hapus-semua"}}); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("aksi tidak dikenal: %d", rr.Code)
	}

	self := e.asAdmin(h, "POST", "/admin/users", url.Values{"action": {"set_active"}, "id": {fmt.Sprint(e.root)}, "active": {"0"}})
	if self.Code != http.StatusUnprocessableEntity || !strings.Contains(admBody(self), "akun Anda sendiri") {
		t.Errorf("admin menonaktifkan diri sendiri: %d", self.Code)
	}
}

func TestAdminAccessFlow(t *testing.T) {
	e := admSetup(t)
	ctx := context.Background()

	// koneksi dibuat lewat halaman; password tidak boleh muncul lagi di mana pun
	form := url.Values{"action": {"create"}, "name": {"DB-A"}, "host": {"127.0.0.1"}, "port": {"5432"},
		"dbname": {"d"}, "username": {"u"}, "password": {"rahasia-koneksi-123"}, "sslmode": {"disable"}}
	if rr := e.asAdmin(e.srv.adminConnections, "POST", "/admin/connections", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("buat koneksi: %d %s", rr.Code, admBody(rr))
	}
	conns, _ := e.svc.ListConnections(ctx)
	if len(conns) != 1 {
		t.Fatalf("koneksi: %+v", conns)
	}
	connID := conns[0].ID.String()

	list := admBody(e.asAdmin(e.srv.adminConnections, "GET", "/admin/connections", nil))
	edit := admBody(e.asAdmin(e.srv.adminConnections, "GET", "/admin/connections?edit="+connID, nil))
	if strings.Contains(list+edit, "rahasia-koneksi-123") || !strings.Contains(edit, "Kosongkan kolom password") {
		t.Error("password koneksi tidak boleh dirender ulang, dan halaman ubah harus tampil")
	}

	bad := url.Values{"action": {"create"}, "name": {"X"}, "host": {"h"}, "port": {"0"}, "dbname": {"d"},
		"username": {"u"}, "password": {"password-tidak-boleh-bocor"}}
	rr := e.asAdmin(e.srv.adminConnections, "POST", "/admin/connections", bad)
	if rr.Code != http.StatusUnprocessableEntity || strings.Contains(admBody(rr), "password-tidak-boleh-bocor") {
		t.Errorf("input salah: %d, password tidak boleh dikembalikan ke form", rr.Code)
	}

	dead := url.Values{"action": {"test"}, "host": {"127.0.0.1"}, "port": {"1"}, "dbname": {"d"}, "username": {"u"}, "password": {"x"}}
	if rr := e.asAdmin(e.srv.adminConnections, "POST", "/admin/connections", dead); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(admBody(rr), "Koneksi gagal") {
		t.Errorf("test connection ke target mati: %d", rr.Code)
	}

	// group + anggota + grant lewat halaman
	if rr := e.asAdmin(e.srv.adminGroups, "POST", "/admin/groups", url.Values{"action": {"create"}, "name": {"tim"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("buat group: %d", rr.Code)
	}
	groups, _ := e.svc.ListGroups(ctx)
	gid := fmt.Sprint(groups[0].ID)
	alice := e.mkUser(t, "alice")
	e.mkSession(t, alice)

	add := e.asAdmin(e.srv.adminGroups, "POST", "/admin/groups", url.Values{"action": {"add_member"}, "group_id": {gid}, "user_id": {fmt.Sprint(alice)}})
	if add.Code != http.StatusSeeOther || add.Header().Get("Location") != "/admin/groups?id="+gid+"&ok=updated" {
		t.Fatalf("tambah anggota: %d %s", add.Code, add.Header().Get("Location"))
	}
	if body := admBody(e.asAdmin(e.srv.adminGroups, "GET", "/admin/groups?id="+gid, nil)); !strings.Contains(body, "alice") {
		t.Error("anggota harus tampil di halaman group")
	}

	if e.forceRotate(t, alice) {
		t.Fatal("belum ada grant: belum perlu rotasi")
	}
	set := url.Values{"action": {"set"}, "group_id": {gid}, "connection_id": {connID}, "level": {"view"}}
	if rr := e.asAdmin(e.srv.adminGrants, "POST", "/admin/grants", set); rr.Code != http.StatusSeeOther {
		t.Fatalf("beri grant: %d %s", rr.Code, admBody(rr))
	}
	if !e.forceRotate(t, alice) {
		t.Error("admin memberi grant: session anggota harus force_rotate")
	}
	if body := admBody(e.asAdmin(e.srv.adminGrants, "GET", "/admin/grants?level=edit", nil)); strings.Contains(body, "Cabut") {
		t.Error("filter edit tidak boleh menampilkan grant view")
	}
	if body := admBody(e.asAdmin(e.srv.adminGrants, "GET", "/admin/grants?level=view", nil)); !strings.Contains(body, "Cabut") {
		t.Error("filter view harus menampilkan grant view")
	}
	if rr := e.asAdmin(e.srv.adminGrants, "POST", "/admin/grants", url.Values{"action": {"set"}, "group_id": {gid}, "connection_id": {connID}, "level": {"root"}}); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("level tidak valid: %d", rr.Code)
	}

	// hapus koneksi
	del := e.asAdmin(e.srv.adminConnections, "POST", "/admin/connections", url.Values{"action": {"delete"}, "id": {connID}})
	if del.Code != http.StatusSeeOther || strings.Contains(admBody(e.asAdmin(e.srv.adminConnections, "GET", "/admin/connections", nil)), "DB-A") {
		t.Error("koneksi terhapus tidak boleh tampil lagi")
	}
}

// Penolakan akses pada halaman database: ini yang diuji dosen (bukan sekadar sembunyi tombol).
func TestDatabaseAccessDenied(t *testing.T) {
	e := admSetup(t)
	ctx := context.Background()

	viewer, editor, outsider := e.mkUser(t, "viewer"), e.mkUser(t, "editor"), e.mkUser(t, "outsider")
	gView, _ := e.svc.CreateGroup(ctx, e.root, "g_view", "")
	gEdit, _ := e.svc.CreateGroup(ctx, e.root, "g_edit", "")
	e.svc.AddMember(ctx, e.root, gView, viewer)
	e.svc.AddMember(ctx, e.root, gEdit, editor)
	conn, err := e.svc.CreateConnection(ctx, e.root, "DB-A", dbconn.Params{Host: "127.0.0.1", Port: 5432, DBName: "d", Username: "u", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.SetGrant(ctx, e.root, gView, conn, "view")
	e.svc.SetGrant(ctx, e.root, gEdit, conn, "edit")

	newMig := url.Values{"name": {"tambah tabel"}, "sql": {"CREATE TABLE t (id int)"}}
	id := conn.String()

	// View Only: boleh melihat riwayat, ditolak menulis
	if rr := e.call(e.srv.migrationList, "GET", "/db/"+id+"/migrations", nil, admPrincipal(viewer, false), id); rr.Code != http.StatusOK {
		t.Errorf("view only melihat riwayat: %d", rr.Code)
	}
	if rr := e.call(e.srv.migrationCreate, "POST", "/db/"+id+"/migrations/new", newMig, admPrincipal(viewer, false), id); rr.Code != http.StatusForbidden {
		t.Errorf("view only membuat migration harus 403, dapat %d", rr.Code)
	}
	if rr := e.call(e.srv.migrationNewForm, "GET", "/db/"+id+"/migrations/new", nil, admPrincipal(viewer, false), id); rr.Code != http.StatusForbidden {
		t.Errorf("view only membuka editor harus 403, dapat %d", rr.Code)
	}

	// View + Edit: boleh membuat draft
	if rr := e.call(e.srv.migrationCreate, "POST", "/db/"+id+"/migrations/new", newMig, admPrincipal(editor, false), id); rr.Code != http.StatusSeeOther {
		t.Errorf("view+edit membuat migration: %d %s", rr.Code, admBody(rr))
	}

	// Tanpa grant, dan Platform Admin tanpa grant: ditolak walau bisa membuka halaman admin
	for name, p := range map[string]*reqctx.Principal{"tanpa grant": admPrincipal(outsider, false), "admin tanpa grant": admPrincipal(e.root, true)} {
		for label, h := range map[string]http.HandlerFunc{"riwayat": e.srv.migrationList, "schema": e.srv.dbSchema} {
			if rr := e.call(h, "GET", "/db/"+id, nil, p, id); rr.Code != http.StatusForbidden {
				t.Errorf("%s ke %s harus 403, dapat %d", name, label, rr.Code)
			}
		}
	}
}
