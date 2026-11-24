package httpx

import (
	"net/http"
	"strconv"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func positiveID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, 404, "not_found", "ID tidak ditemukan.")
		return 0, false
	}
	return id, true
}

func connectionID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("cid"))
	if err != nil {
		apiError(w, 404, "not_found", "Koneksi tidak ditemukan.")
		return uuid.Nil, false
	}
	return id, true
}

func actor(r *http.Request) reqctx.Principal { p, _ := reqctx.CurrentPrincipal(r.Context()); return p }

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Admin.ListUsers(r.Context())
	if err != nil {
		apiFailure(w, err)
		return
	}
	result := []map[string]any{}
	for _, u := range users {
		result = append(result, map[string]any{"id": u.ID, "username": u.Username, "is_platform_admin": u.IsPlatformAdmin, "is_active": u.IsActive, "created_at": u.CreatedAt})
	}
	apiData(w, result)
}

func (s *Server) apiUserCreate(w http.ResponseWriter, r *http.Request) {
	var f struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Admin    bool   `json:"is_platform_admin"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	id, err := s.Admin.CreateUser(r.Context(), actor(r).UserID, f.Username, f.Password, f.Admin)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiJSON(w, 201, map[string]any{"data": map[string]any{"id": id}})
}

func (s *Server) apiUserUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := positiveID(w, r, "uid")
	if !ok {
		return
	}
	var f struct {
		Active *bool `json:"is_active"`
		Admin  *bool `json:"is_platform_admin"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	if (f.Active == nil) == (f.Admin == nil) {
		apiError(w, 422, "validation", "Ubah tepat satu atribut pengguna per permintaan.")
		return
	}
	var err error
	if f.Active != nil {
		err = s.Admin.SetUserActive(r.Context(), actor(r).UserID, id, *f.Active)
	} else {
		err = s.Admin.SetAdmin(r.Context(), actor(r).UserID, id, *f.Admin)
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

func (s *Server) apiUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := positiveID(w, r, "uid")
	if !ok {
		return
	}
	var f struct {
		Password string `json:"password"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	if err := s.Admin.ResetPassword(r.Context(), actor(r).UserID, id, f.Password); err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

func (s *Server) apiGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.Admin.ListGroups(r.Context())
	if err != nil {
		apiFailure(w, err)
		return
	}
	leaders := map[int64]contracts.Leader{}
	if s.Leaders != nil {
		list, err := s.Leaders.List(r.Context(), actor(r))
		if err != nil {
			apiFailure(w, err)
			return
		}
		for _, l := range list {
			leaders[l.GroupID] = l
		}
	}
	result := []map[string]any{}
	for _, g := range groups {
		var leader any
		if l, ok := leaders[g.ID]; ok {
			leader = l
		}
		result = append(result, map[string]any{"id": g.ID, "name": g.Name, "description": g.Description, "members": g.Members, "leader": leader})
	}
	apiData(w, result)
}

func (s *Server) apiGroupCreate(w http.ResponseWriter, r *http.Request) {
	var f struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	if len(f.Name) > 200 || len(f.Description) > 4000 {
		apiError(w, 422, "validation", "Nama atau deskripsi group terlalu panjang.")
		return
	}
	id, err := s.Admin.CreateGroup(r.Context(), actor(r).UserID, f.Name, f.Description)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiJSON(w, 201, map[string]any{"data": map[string]any{"id": id}})
}

func (s *Server) apiMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := positiveID(w, r, "gid")
	if !ok {
		return
	}
	var exists bool
	if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM groups WHERE id=$1)", id).Scan(&exists); err != nil {
		apiFailure(w, err)
		return
	}
	if !exists {
		apiFailure(w, admin.ErrNotFound)
		return
	}
	list, err := s.Admin.Members(r.Context(), id)
	if err != nil {
		apiFailure(w, err)
		return
	}
	result := []map[string]any{}
	for _, m := range list {
		result = append(result, map[string]any{"user_id": m.UserID, "username": m.Username})
	}
	apiData(w, result)
}

func (s *Server) apiMemberAdd(w http.ResponseWriter, r *http.Request) {
	id, ok := positiveID(w, r, "gid")
	if !ok {
		return
	}
	var f struct {
		UserID int64 `json:"user_id"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	if f.UserID <= 0 {
		apiError(w, 422, "validation", "Pilih pengguna yang valid.")
		return
	}
	if err := s.Admin.AddMember(r.Context(), actor(r).UserID, id, f.UserID); err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

func (s *Server) apiMemberRemove(w http.ResponseWriter, r *http.Request) {
	g, ok := positiveID(w, r, "gid")
	if !ok {
		return
	}
	u, ok := positiveID(w, r, "uid")
	if !ok {
		return
	}
	if err := s.Admin.RemoveMember(r.Context(), actor(r).UserID, g, u); err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

func (s *Server) apiLeader(w http.ResponseWriter, r *http.Request) {
	g, ok := positiveID(w, r, "gid")
	if !ok {
		return
	}
	var f struct {
		UserID int64 `json:"user_id"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	if f.UserID <= 0 {
		apiError(w, 422, "validation", "Pilih anggota group yang valid.")
		return
	}
	if s.Leaders == nil {
		moduleUnavailable(w, "leader group")
		return
	}
	if err := s.Leaders.Set(r.Context(), actor(r), g, f.UserID); err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

type connectionForm struct {
	Name     string      `json:"name"`
	Host     string      `json:"host"`
	Port     int         `json:"port"`
	DBName   string      `json:"dbname"`
	Username string      `json:"username"`
	Password string      `json:"password"`
	SSLMode  string      `json:"sslmode"`
	DBMS     dbconn.DBMS `json:"dbms"`
}

func (f connectionForm) params() dbconn.Params {
	return dbconn.Params{Host: f.Host, Port: f.Port, DBName: f.DBName, Username: f.Username, Password: f.Password, SSLMode: f.SSLMode, DBMS: f.DBMS}
}

func (s *Server) apiConnections(w http.ResponseWriter, r *http.Request) {
	list, err := s.Admin.ListConnections(r.Context())
	if err != nil {
		apiFailure(w, err)
		return
	}
	result := []map[string]any{}
	for _, c := range list {
		result = append(result, map[string]any{"id": c.ID, "name": c.Name, "host": c.Host, "port": c.Port, "dbname": c.DBName, "username": c.Username, "sslmode": c.SSLMode, "dbms": c.DBMS})
	}
	apiData(w, result)
}

func (s *Server) apiConnectionSave(w http.ResponseWriter, r *http.Request) {
	var f connectionForm
	if !apiDecode(w, r, &f) {
		return
	}
	var id uuid.UUID
	var err error
	if r.Method == http.MethodPost {
		id, err = s.Admin.CreateConnection(r.Context(), actor(r).UserID, f.Name, f.params())
	} else {
		var ok bool
		id, ok = connectionID(w, r)
		if !ok {
			return
		}
		err = s.Admin.UpdateConnection(r.Context(), actor(r).UserID, id, f.Name, f.params())
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	apiJSON(w, status, map[string]any{"data": map[string]any{"id": id}})
}

func (s *Server) apiConnectionDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := connectionID(w, r)
	if !ok {
		return
	}
	if err := s.Admin.DeleteConnection(r.Context(), actor(r).UserID, id); err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

func (s *Server) apiConnectionTest(w http.ResponseWriter, r *http.Request) {
	var f struct {
		connectionForm
		ConnectionID string `json:"connection_id"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	if f.ConnectionID != "" {
		id, err := uuid.Parse(f.ConnectionID)
		if err != nil {
			apiFailure(w, admin.ErrNotFound)
			return
		}
		var blob []byte
		var dbms dbconn.DBMS
		err = s.DB.QueryRow(r.Context(), "SELECT password_enc,dbms FROM db_connections WHERE id=$1 AND deleted_at IS NULL", id).Scan(&blob, &dbms)
		if err == pgx.ErrNoRows {
			apiFailure(w, admin.ErrNotFound)
			return
		}
		if err != nil {
			apiFailure(w, err)
			return
		}
		if f.DBMS != dbms {
			apiError(w, 422, "validation", "Jenis DBMS koneksi tersimpan tidak dapat diubah.")
			return
		}
		if f.Password == "" {
			f.Password, err = s.Admin.Conns.Unseal(id, blob)
			if err != nil {
				apiFailure(w, err)
				return
			}
		}
	}
	if err := s.Admin.TestConnection(r.Context(), f.params()); err != nil {
		apiError(w, 422, "connection_failed", "Koneksi gagal. Periksa DBMS, host, port, kredensial, dan TLS.")
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

func (s *Server) apiGrants(w http.ResponseWriter, r *http.Request) {
	list, err := s.Admin.ListGrants(r.Context())
	if err != nil {
		apiFailure(w, err)
		return
	}
	result := []map[string]any{}
	for _, g := range list {
		result = append(result, map[string]any{"group_id": g.GroupID, "group_name": g.GroupName, "connection_id": g.ConnectionID, "connection_name": g.ConnectionName, "level": g.Level})
	}
	apiData(w, result)
}

func (s *Server) apiGrantSave(w http.ResponseWriter, r *http.Request) {
	var f struct {
		GroupID      int64  `json:"group_id"`
		ConnectionID string `json:"connection_id"`
		Level        string `json:"level"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	id, err := uuid.Parse(f.ConnectionID)
	if err != nil || f.GroupID <= 0 {
		apiError(w, 422, "validation", "Pilih group dan koneksi yang valid.")
		return
	}
	if err := s.Admin.SetGrant(r.Context(), actor(r).UserID, f.GroupID, id, f.Level); err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}

func (s *Server) apiGrantRemove(w http.ResponseWriter, r *http.Request) {
	g, ok := positiveID(w, r, "gid")
	if !ok {
		return
	}
	id, ok := connectionID(w, r)
	if !ok {
		return
	}
	if err := s.Admin.RemoveGrant(r.Context(), actor(r).UserID, g, id); err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, map[string]bool{"ok": true})
}
