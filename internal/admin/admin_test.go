package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/user"
)

type env struct {
	db    *pgxpool.Pool
	svc   *Service
	az    *authz.Authorizer
	key   []byte
	actor int64 // platform admin yang melakukan semua aksi
}

func setup(t *testing.T, name string) *env {
	t.Helper()
	ctx := context.Background()
	db := testdb.Fresh(t, name)
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		DB:       db,
		Users:    &user.Repo{DB: db},
		Sessions: &auth.Sessions{DB: db},
		Conns:    &dbconn.Registry{DB: db, Key: key},
		Hash:     func(pw string) (string, error) { return "hash:" + pw, nil },
	}
	actor, err := svc.Users.Create(ctx, "root", "h", true)
	if err != nil {
		t.Fatal(err)
	}
	return &env{db: db, svc: svc, az: &authz.Authorizer{DB: db}, key: key, actor: actor}
}

func (e *env) newUser(t *testing.T, name string) int64 {
	t.Helper()
	id, err := e.svc.CreateUser(context.Background(), e.actor, name, "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) newSession(t *testing.T, userID int64) {
	t.Helper()
	_, err := e.db.Exec(context.Background(), `
		INSERT INTO sessions (user_id, token_hash, csrf_token, absolute_expires_at)
		VALUES ($1, $2, 'c', now() + interval '8 hours')`, userID, []byte(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) forceRotate(t *testing.T, userID int64) bool {
	t.Helper()
	var v bool
	if err := e.db.QueryRow(context.Background(),
		`SELECT force_rotate FROM sessions WHERE user_id = $1`, userID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) clearRotate(t *testing.T) {
	t.Helper()
	if _, err := e.db.Exec(context.Background(), `UPDATE sessions SET force_rotate = false`); err != nil {
		t.Fatal(err)
	}
}

func (e *env) can(userID int64, connID uuid.UUID, act authz.Action) bool {
	return e.az.Can(context.Background(), reqctx.Principal{UserID: userID}, connID, act) == nil
}

func (e *env) newConn(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id, err := e.svc.CreateConnection(context.Background(), e.actor, name, dbconn.Params{
		Host: "127.0.0.1", Port: 5432, DBName: "d", Username: "u", Password: "s3cret-pass"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	e := setup(t, "admin_users")

	id := e.newUser(t, "budi")
	var stored string
	if err := e.db.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "password123" {
		t.Error("password tidak boleh disimpan sebagai plaintext")
	}

	for name, fn := range map[string]func() error{
		"username kembar (tak peduli huruf besar)": func() error { _, err := e.svc.CreateUser(ctx, e.actor, "BUDI", "password123", false); return err },
		"password terlalu pendek":                  func() error { _, err := e.svc.CreateUser(ctx, e.actor, "x", "pendek", false); return err },
		"username kosong":                          func() error { _, err := e.svc.CreateUser(ctx, e.actor, "  ", "password123", false); return err },
	} {
		if err := fn(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: harusnya ErrInvalid, dapat %v", name, err)
		}
	}

	if err := e.svc.SetUserActive(ctx, e.actor, e.actor, false); !errors.Is(err, ErrSelf) {
		t.Errorf("admin menonaktifkan diri sendiri: %v", err)
	}
	if err := e.svc.SetAdmin(ctx, e.actor, e.actor, false); !errors.Is(err, ErrSelf) {
		t.Errorf("admin mencabut admin diri sendiri: %v", err)
	}
	if err := e.svc.SetUserActive(ctx, e.actor, 9999, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("user tidak ada: %v", err)
	}

	e.newSession(t, id)
	if err := e.svc.SetUserActive(ctx, e.actor, id, false); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	e.db.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM sessions WHERE user_id = $1`, id).Scan(&revoked)
	if !revoked {
		t.Error("menonaktifkan user harus mencabut session-nya di backend")
	}

	id2 := e.newUser(t, "ani")
	e.newSession(t, id2)
	if err := e.svc.ResetPassword(ctx, e.actor, id2, "passwordbaru1"); err != nil {
		t.Fatal(err)
	}
	e.db.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM sessions WHERE user_id = $1`, id2).Scan(&revoked)
	if !revoked {
		t.Error("reset password harus mencabut session lama")
	}

	id3 := e.newUser(t, "cici")
	e.newSession(t, id3)
	if err := e.svc.SetAdmin(ctx, e.actor, id3, true); err != nil {
		t.Fatal(err)
	}
	if !e.forceRotate(t, id3) {
		t.Error("user dijadikan admin: session harus force_rotate (privilege naik)")
	}

	var n int
	e.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'user.create'`).Scan(&n)
	if n < 3 {
		t.Errorf("audit log user.create: %d", n)
	}
}

func TestAccessFlow(t *testing.T) {
	ctx := context.Background()
	e := setup(t, "admin_access")
	alice, bob := e.newUser(t, "alice"), e.newUser(t, "bob")
	e.newSession(t, alice)
	e.newSession(t, bob)

	gid, err := e.svc.CreateGroup(ctx, e.actor, "tim", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateGroup(ctx, e.actor, "tim", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("nama group kembar: %v", err)
	}
	conn := e.newConn(t, "DB-A")

	// password koneksi terenkripsi, bisa didekripsi dengan kunci + id yang benar
	var enc []byte
	e.db.QueryRow(ctx, `SELECT password_enc FROM db_connections WHERE id = $1`, conn).Scan(&enc)
	if bytes.Contains(enc, []byte("s3cret-pass")) {
		t.Fatal("password koneksi tersimpan sebagai plaintext")
	}
	if pw, err := dbconn.Decrypt(e.key, conn, enc); err != nil || pw != "s3cret-pass" {
		t.Fatalf("dekripsi: %q, %v", pw, err)
	}

	// tanpa grant: ditolak, termasuk platform admin
	if e.can(alice, conn, authz.SchemaView) {
		t.Fatal("tanpa grant harus ditolak")
	}
	if e.can(e.actor, conn, authz.SchemaView) {
		t.Fatal("platform admin tanpa grant harus ditolak")
	}

	// masuk group yang belum punya grant: privilege tidak naik
	if err := e.svc.AddMember(ctx, e.actor, gid, alice); err != nil {
		t.Fatal(err)
	}
	if e.forceRotate(t, alice) {
		t.Error("group tanpa grant: tidak perlu rotasi")
	}

	// grant view: naik dari none -> rotasi, bisa baca, tidak bisa menulis
	if err := e.svc.SetGrant(ctx, e.actor, gid, conn, "view"); err != nil {
		t.Fatal(err)
	}
	if !e.forceRotate(t, alice) {
		t.Error("grant baru harus force_rotate anggota group")
	}
	if !e.can(alice, conn, authz.SchemaView) || e.can(alice, conn, authz.MigrationCreate) {
		t.Error("view only: boleh baca, tidak boleh menulis")
	}

	// view -> view tidak merotasi; view -> edit merotasi; edit -> view tidak merotasi
	e.clearRotate(t)
	e.svc.SetGrant(ctx, e.actor, gid, conn, "view")
	if e.forceRotate(t, alice) {
		t.Error("level tidak berubah: tidak perlu rotasi")
	}
	e.svc.SetGrant(ctx, e.actor, gid, conn, "edit")
	if !e.forceRotate(t, alice) || !e.can(alice, conn, authz.MigrationExecute) {
		t.Error("view -> edit: harus rotasi dan boleh execute")
	}
	e.clearRotate(t)
	e.svc.SetGrant(ctx, e.actor, gid, conn, "view")
	if e.forceRotate(t, alice) {
		t.Error("penurunan level tidak perlu rotasi")
	}
	if e.can(alice, conn, authz.MigrationExecute) {
		t.Error("penurunan level berlaku langsung pada request berikutnya")
	}

	// anggota baru ke group yang sudah punya grant: rotasi; dikeluarkan: langsung ditolak
	if err := e.svc.AddMember(ctx, e.actor, gid, bob); err != nil {
		t.Fatal(err)
	}
	if !e.forceRotate(t, bob) || !e.can(bob, conn, authz.SchemaView) {
		t.Error("anggota baru: rotasi dan boleh baca")
	}
	if err := e.svc.RemoveMember(ctx, e.actor, gid, bob); err != nil {
		t.Fatal(err)
	}
	if e.can(bob, conn, authz.SchemaView) {
		t.Error("dikeluarkan dari group: harus langsung ditolak")
	}

	grants, _ := e.svc.ListGrants(ctx)
	if len(grants) != 1 || grants[0].Level != "view" || grants[0].ConnectionName != "DB-A" {
		t.Errorf("ListGrants: %+v", grants)
	}
	if err := e.svc.RemoveGrant(ctx, e.actor, gid, conn); err != nil {
		t.Fatal(err)
	}
	if e.can(alice, conn, authz.SchemaView) {
		t.Error("grant dicabut: harus ditolak")
	}

	// validasi input
	if err := e.svc.SetGrant(ctx, e.actor, gid, conn, "admin"); !errors.Is(err, ErrInvalid) {
		t.Errorf("level tidak valid: %v", err)
	}
	if err := e.svc.SetGrant(ctx, e.actor, gid, uuid.New(), "view"); !errors.Is(err, ErrNotFound) {
		t.Errorf("koneksi tidak ada: %v", err)
	}
}

func TestConnections(t *testing.T) {
	ctx := context.Background()
	e := setup(t, "admin_conns")
	conn := e.newConn(t, "DB-A")
	good := dbconn.Params{Host: "h", Port: 5432, DBName: "d", Username: "u", Password: "x"}

	bad := map[string]func(p *dbconn.Params){
		"port 0":             func(p *dbconn.Params) { p.Port = 0 },
		"port terlalu besar": func(p *dbconn.Params) { p.Port = 70000 },
		"sslmode aneh":       func(p *dbconn.Params) { p.SSLMode = "bebas" },
		"host kosong":        func(p *dbconn.Params) { p.Host = "" },
		"password kosong":    func(p *dbconn.Params) { p.Password = "" },
	}
	for name, mut := range bad {
		p := good
		mut(&p)
		if _, err := e.svc.CreateConnection(ctx, e.actor, "X-"+name, p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: harusnya ErrInvalid, dapat %v", name, err)
		}
	}
	if _, err := e.svc.CreateConnection(ctx, e.actor, "DB-A", good); !errors.Is(err, ErrInvalid) {
		t.Errorf("nama koneksi kembar: %v", err)
	}

	// ubah tanpa password: password lama dipertahankan; dengan password: diganti
	readPW := func() string {
		var enc []byte
		e.db.QueryRow(ctx, `SELECT password_enc FROM db_connections WHERE id = $1`, conn).Scan(&enc)
		pw, _ := dbconn.Decrypt(e.key, conn, enc)
		return pw
	}
	upd := dbconn.Params{Host: "baru", Port: 5433, DBName: "d2", Username: "u2"}
	if err := e.svc.UpdateConnection(ctx, e.actor, conn, "DB-A2", upd); err != nil {
		t.Fatal(err)
	}
	if readPW() != "s3cret-pass" {
		t.Error("password kosong saat ubah: password lama harus tetap")
	}
	upd.Password = "pw-baru"
	if err := e.svc.UpdateConnection(ctx, e.actor, conn, "DB-A2", upd); err != nil {
		t.Fatal(err)
	}
	if readPW() != "pw-baru" {
		t.Error("password baru harus tersimpan terenkripsi")
	}

	// hapus = soft delete: hilang dari daftar; nama bisa dipakai lagi
	if err := e.svc.DeleteConnection(ctx, e.actor, conn); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.ListConnections(ctx); len(list) != 0 {
		t.Errorf("koneksi terhapus masih tampil: %+v", list)
	}
	if err := e.svc.DeleteConnection(ctx, e.actor, conn); !errors.Is(err, ErrNotFound) {
		t.Errorf("hapus dua kali: %v", err)
	}
	e.newConn(t, "DB-A2")

	// test connection ke port tertutup gagal
	if err := e.svc.TestConnection(ctx, dbconn.Params{Host: "127.0.0.1", Port: 1, DBName: "d", Username: "u"}); err == nil {
		t.Error("test connection ke port tertutup harus gagal")
	}
}

func TestEnsureAdmin(t *testing.T) {
	ctx := context.Background()
	e := setup(t, "admin_boot") // setup sudah membuat satu admin ("root")

	created, err := e.svc.Users.EnsureAdmin(ctx, "admin2", "h")
	if err != nil || created {
		t.Errorf("sudah ada admin: created=%v err=%v", created, err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE users SET is_platform_admin = false`); err != nil {
		t.Fatal(err)
	}
	created, err = e.svc.Users.EnsureAdmin(ctx, "admin2", "h")
	if err != nil || !created {
		t.Errorf("belum ada admin: created=%v err=%v", created, err)
	}
}
