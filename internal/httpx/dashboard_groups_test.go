package httpx

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

// User melihat group miliknya sendiri di dashboard, tetapi tidak group atau anggota milik orang lain.
func TestDashboardShowsOwnGroupsOnly(t *testing.T) {
	e := admSetup(t)
	ctx := context.Background()

	viewer, editor := e.mkUser(t, "pengguna_lihat"), e.mkUser(t, "pengguna_ubah")
	gView, _ := e.svc.CreateGroup(ctx, e.root, "tim_lihat", "")
	gEdit, _ := e.svc.CreateGroup(ctx, e.root, "tim_ubah", "")
	e.svc.AddMember(ctx, e.root, gView, viewer)
	e.svc.AddMember(ctx, e.root, gEdit, editor)
	conn, err := e.svc.CreateConnection(ctx, e.root, "DB-A", dbconn.Params{Host: "127.0.0.1", Port: 5432, DBName: "d", Username: "u", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.SetGrant(ctx, e.root, gView, conn, "view")
	e.svc.SetGrant(ctx, e.root, gEdit, conn, "edit")

	rr := e.call(e.srv.dashboard, "GET", "/dashboard", nil, admPrincipal(viewer, false), "")
	body := admBody(rr)
	if rr.Code != http.StatusOK || !strings.Contains(body, "View Only") || !strings.Contains(body, "lewat group: tim_lihat") {
		t.Fatalf("dashboard view only harus menampilkan group sendiri: %d\n%s", rr.Code, body)
	}
	if strings.Contains(body, "tim_ubah") || strings.Contains(body, "pengguna_ubah") {
		t.Error("group atau anggota milik user lain tidak boleh tampil")
	}

	rr = e.call(e.srv.dashboard, "GET", "/dashboard", nil, admPrincipal(editor, false), "")
	body = admBody(rr)
	if rr.Code != http.StatusOK || !strings.Contains(body, "View + Edit") || !strings.Contains(body, "lewat group: tim_ubah") {
		t.Fatalf("dashboard view+edit harus menampilkan group sendiri: %d\n%s", rr.Code, body)
	}
	if strings.Contains(body, "tim_lihat") || strings.Contains(body, "pengguna_lihat") {
		t.Error("group atau anggota milik user lain tidak boleh tampil")
	}
}
