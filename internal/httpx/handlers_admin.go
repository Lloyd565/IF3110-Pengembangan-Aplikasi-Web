package httpx

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

// Halaman Platform Admin. Setiap halaman satu URL: GET menampilkan, POST
// menjalankan aksi lewat field tersembunyi "action", lalu redirect (PRG).

const maxAdminForm = 64 << 10

type adminFlash struct{ OK, Error string }

// adminOKMessages: pesan sukses hanya dari daftar tetap (nilai ?ok=), bukan teks bebas dari URL.
var adminOKMessages = map[string]string{
	"created": "Berhasil dibuat.",
	"updated": "Perubahan disimpan.",
	"deleted": "Berhasil dihapus.",
}

var (
	errAdminAction = fmt.Errorf("%w: aksi tidak dikenal", admin.ErrInvalid)
	adminSSLModes  = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}
)

// adminGuard: pemeriksaan di handler sendiri (berlapis dengan requireAdmin di route).
func (s *Server) adminGuard(w http.ResponseWriter, r *http.Request) (reqctx.Principal, bool) {
	p, ok := reqctx.CurrentPrincipal(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return p, false
	}
	if !p.IsPlatformAdmin {
		s.renderError(w, r, http.StatusForbidden, "Halaman ini hanya untuk Platform Admin.")
		return p, false
	}
	if s.Admin == nil { // service belum dipasang: perilaku sama dengan stub lama (501)
		notImplemented(w, r)
		return p, false
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxAdminForm)
		if err := r.ParseForm(); err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Form tidak valid atau terlalu besar.")
			return p, false
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		s.renderError(w, r, http.StatusMethodNotAllowed, "Metode tidak didukung.")
		return p, false
	}
	return p, true
}

// adminFlashFor: ubah hasil aksi menjadi pesan + status HTTP. Error tak terduga hanya dicatat di log.
func adminFlashFor(r *http.Request, err error) (adminFlash, int) {
	if err == nil {
		return adminFlash{OK: adminOKMessages[r.URL.Query().Get("ok")]}, http.StatusOK
	}
	switch {
	case errors.Is(err, admin.ErrInvalid):
		return adminFlash{Error: strings.TrimPrefix(err.Error(), admin.ErrInvalid.Error()+": ")}, http.StatusUnprocessableEntity
	case errors.Is(err, admin.ErrNotFound):
		return adminFlash{Error: "Data tidak ditemukan."}, http.StatusNotFound
	case errors.Is(err, admin.ErrSelf):
		return adminFlash{Error: "Aksi ini tidak boleh dilakukan pada akun Anda sendiri."}, http.StatusUnprocessableEntity
	}
	log.Printf("admin %s: %v", r.URL.Path, err)
	return adminFlash{Error: "Terjadi kesalahan saat memproses permintaan."}, http.StatusInternalServerError
}

func (s *Server) adminLoadFailed(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("admin %s: %v", r.URL.Path, err)
	s.renderError(w, r, http.StatusInternalServerError, "Gagal memuat data.")
}

func adminRedirect(w http.ResponseWriter, r *http.Request, base, code string) {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	http.Redirect(w, r, base+sep+"ok="+code, http.StatusSeeOther)
}

func formID(r *http.Request, key string) int64 {
	id, _ := strconv.ParseInt(r.PostFormValue(key), 10, 64)
	return id
}

func formUUID(r *http.Request, key string) uuid.UUID {
	id, _ := uuid.Parse(r.PostFormValue(key))
	return id
}

// ---- /admin/users ----

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := s.adminGuard(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		ctx, code := r.Context(), "updated"
		var err error
		switch r.PostFormValue("action") {
		case "create":
			code = "created"
			_, err = s.Admin.CreateUser(ctx, p.UserID, r.PostFormValue("username"), r.PostFormValue("password"), r.PostFormValue("is_admin") != "")
		case "set_active":
			err = s.Admin.SetUserActive(ctx, p.UserID, formID(r, "id"), r.PostFormValue("active") == "1")
		case "set_admin":
			err = s.Admin.SetAdmin(ctx, p.UserID, formID(r, "id"), r.PostFormValue("admin") == "1")
		case "reset_password":
			err = s.Admin.ResetPassword(ctx, p.UserID, formID(r, "id"), r.PostFormValue("password"))
		default:
			err = errAdminAction
		}
		if err != nil {
			s.renderUsers(w, r, p, err)
			return
		}
		adminRedirect(w, r, "/admin/users", code)
		return
	}
	s.renderUsers(w, r, p, nil)
}

func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, p reqctx.Principal, actErr error) {
	users, err := s.Admin.ListUsers(r.Context())
	if err != nil {
		s.adminLoadFailed(w, r, err)
		return
	}
	fl, status := adminFlashFor(r, actErr)
	s.render(w, r, status, "admin-users.html", map[string]any{"Users": users, "Flash": fl, "Self": p.UserID})
}

// ---- /admin/groups ----

func (s *Server) adminGroups(w http.ResponseWriter, r *http.Request) {
	p, ok := s.adminGuard(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		ctx, code := r.Context(), "updated"
		gid := formID(r, "group_id")
		back := fmt.Sprintf("/admin/groups?id=%d", gid)
		var err error
		switch r.PostFormValue("action") {
		case "create":
			code, back = "created", "/admin/groups"
			_, err = s.Admin.CreateGroup(ctx, p.UserID, r.PostFormValue("name"), r.PostFormValue("description"))
		case "add_member":
			err = s.Admin.AddMember(ctx, p.UserID, gid, formID(r, "user_id"))
		case "remove_member":
			err = s.Admin.RemoveMember(ctx, p.UserID, gid, formID(r, "user_id"))
		default:
			err = errAdminAction
		}
		if err != nil {
			s.renderGroups(w, r, err, gid)
			return
		}
		adminRedirect(w, r, back, code)
		return
	}
	sel, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	s.renderGroups(w, r, nil, sel)
}

func (s *Server) renderGroups(w http.ResponseWriter, r *http.Request, actErr error, sel int64) {
	ctx := r.Context()
	groups, err := s.Admin.ListGroups(ctx)
	if err != nil {
		s.adminLoadFailed(w, r, err)
		return
	}
	users, err := s.Admin.ListUsers(ctx)
	if err != nil {
		s.adminLoadFailed(w, r, err)
		return
	}
	var selected *admin.Group
	var members []admin.Member
	for i := range groups {
		if groups[i].ID == sel {
			selected = &groups[i]
		}
	}
	if selected != nil {
		if members, err = s.Admin.Members(ctx, selected.ID); err != nil {
			s.adminLoadFailed(w, r, err)
			return
		}
	}
	fl, status := adminFlashFor(r, actErr)
	s.render(w, r, status, "admin-groups.html", map[string]any{
		"Groups": groups, "Selected": selected, "Members": members, "Users": users, "Flash": fl,
	})
}

// ---- /admin/connections ----

type adminConnForm struct{ Name, Host, Port, DBName, Username, SSLMode string }

func (f adminConnForm) params(password string) dbconn.Params {
	port, _ := strconv.Atoi(f.Port) // tidak valid -> 0 -> ditolak validasi service
	return dbconn.Params{Host: f.Host, Port: port, DBName: f.DBName, Username: f.Username, Password: password, SSLMode: f.SSLMode}
}

func (s *Server) adminConnections(w http.ResponseWriter, r *http.Request) {
	p, ok := s.adminGuard(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		ctx := r.Context()
		form := adminConnForm{
			Name: r.PostFormValue("name"), Host: r.PostFormValue("host"), Port: r.PostFormValue("port"),
			DBName: r.PostFormValue("dbname"), Username: r.PostFormValue("username"), SSLMode: r.PostFormValue("sslmode"),
		}
		password, id := r.PostFormValue("password"), r.PostFormValue("id")
		code := "updated"
		var err error
		switch r.PostFormValue("action") {
		case "create":
			code = "created"
			_, err = s.Admin.CreateConnection(ctx, p.UserID, form.Name, form.params(password))
		case "update":
			err = s.Admin.UpdateConnection(ctx, p.UserID, formUUID(r, "id"), form.Name, form.params(password))
		case "delete":
			code = "deleted"
			err = s.Admin.DeleteConnection(ctx, p.UserID, formUUID(r, "id"))
		case "test":
			err = s.Admin.TestConnection(ctx, form.params(password))
			if err == nil {
				s.renderConnections(w, r, adminFlash{OK: "Koneksi berhasil."}, http.StatusOK, &form, id)
				return
			}
			if !errors.Is(err, admin.ErrInvalid) {
				s.renderConnections(w, r, adminFlash{Error: "Koneksi gagal: " + err.Error()}, http.StatusUnprocessableEntity, &form, id)
				return
			}
		default:
			err = errAdminAction
		}
		if err != nil {
			fl, status := adminFlashFor(r, err)
			s.renderConnections(w, r, fl, status, &form, id) // password sengaja tidak dikembalikan ke form
			return
		}
		adminRedirect(w, r, "/admin/connections", code)
		return
	}
	fl, status := adminFlashFor(r, nil)
	s.renderConnections(w, r, fl, status, nil, r.URL.Query().Get("edit"))
}

func (s *Server) renderConnections(w http.ResponseWriter, r *http.Request, fl adminFlash, status int, form *adminConnForm, editID string) {
	conns, err := s.Admin.ListConnections(r.Context())
	if err != nil {
		s.adminLoadFailed(w, r, err)
		return
	}
	var edit *admin.Connection
	for i := range conns {
		if conns[i].ID.String() == editID {
			edit = &conns[i]
		}
	}
	f := adminConnForm{Port: "5432", SSLMode: "disable"}
	if edit != nil {
		f = adminConnForm{edit.Name, edit.Host, strconv.Itoa(edit.Port), edit.DBName, edit.Username, edit.SSLMode}
	}
	if form != nil {
		f = *form
	}
	s.render(w, r, status, "admin-connections.html", map[string]any{
		"Conns": conns, "Edit": edit, "Form": f, "Flash": fl, "SSLModes": adminSSLModes,
	})
}

// ---- /admin/grants ----

func (s *Server) adminGrants(w http.ResponseWriter, r *http.Request) {
	p, ok := s.adminGuard(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		ctx, code := r.Context(), "updated"
		var err error
		switch r.PostFormValue("action") {
		case "set":
			err = s.Admin.SetGrant(ctx, p.UserID, formID(r, "group_id"), formUUID(r, "connection_id"), r.PostFormValue("level"))
		case "remove":
			code = "deleted"
			err = s.Admin.RemoveGrant(ctx, p.UserID, formID(r, "group_id"), formUUID(r, "connection_id"))
		default:
			err = errAdminAction
		}
		if err != nil {
			s.renderGrants(w, r, err)
			return
		}
		adminRedirect(w, r, "/admin/grants", code)
		return
	}
	s.renderGrants(w, r, nil)
}

func (s *Server) renderGrants(w http.ResponseWriter, r *http.Request, actErr error) {
	ctx := r.Context()
	grants, err := s.Admin.ListGrants(ctx)
	if err != nil {
		s.adminLoadFailed(w, r, err)
		return
	}
	groups, err := s.Admin.ListGroups(ctx)
	if err != nil {
		s.adminLoadFailed(w, r, err)
		return
	}
	conns, err := s.Admin.ListConnections(ctx)
	if err != nil {
		s.adminLoadFailed(w, r, err)
		return
	}
	level := r.URL.Query().Get("level")
	if level != "view" && level != "edit" {
		level = ""
	}
	if level != "" {
		filtered := grants[:0:0]
		for _, g := range grants {
			if g.Level == level {
				filtered = append(filtered, g)
			}
		}
		grants = filtered
	}
	fl, status := adminFlashFor(r, actErr)
	s.render(w, r, status, "admin-grants.html", map[string]any{
		"Grants": grants, "Groups": groups, "Conns": conns, "Level": level, "Flash": fl,
	})
}
