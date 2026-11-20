package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/explorer"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
)

func isAPI(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api"
}

func apiJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func apiData(w http.ResponseWriter, value any) {
	apiJSON(w, http.StatusOK, map[string]any{"data": value})
}

func apiError(w http.ResponseWriter, status int, code, message string) {
	apiJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func apiDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		apiError(w, 415, "content_type", "Gunakan Content-Type application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		status := http.StatusBadRequest
		if errors.As(err, new(*http.MaxBytesError)) {
			status = http.StatusRequestEntityTooLarge
		}
		apiError(w, status, "invalid_json", "Body JSON tidak valid atau terlalu besar.")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		apiError(w, 400, "invalid_json", "Body harus berisi satu objek JSON.")
		return false
	}
	return true
}

func apiFailure(w http.ResponseWriter, err error) {
	var service *contracts.Error
	var invalid migration.ValidationError
	switch {
	case errors.As(err, &service):
		if service.Status < 400 || service.Status > 599 {
			apiError(w, 500, "internal", "Layanan tidak dapat memproses permintaan.")
			return
		}
		apiError(w, service.Status, service.Code, service.Message)
	case errors.Is(err, admin.ErrInvalid):
		apiError(w, 422, "validation", strings.TrimPrefix(err.Error(), "admin: input tidak valid: "))
	case errors.Is(err, admin.ErrSelf):
		apiError(w, 422, "self_change", "Aksi ini tidak diizinkan untuk akun Anda sendiri.")
	case errors.Is(err, admin.ErrNotFound), errors.Is(err, migration.ErrNotFound), errors.Is(err, explorer.ErrNotFound):
		apiError(w, 404, "not_found", "Data tidak ditemukan.")
	case errors.Is(err, migration.ErrNotDraft):
		apiError(w, 409, "immutable", "Migration yang pernah dieksekusi tidak dapat diubah.")
	case errors.As(err, &invalid):
		apiError(w, 422, "validation", invalid.Error())
	case errors.Is(err, auth.ErrInvalidSession):
		apiError(w, 401, "unauthenticated", "Silakan login kembali.")
	default:
		slog.Error("api.operation_failed")
		apiError(w, 503, "unavailable", "Layanan belum dapat memproses permintaan. Coba lagi nanti.")
	}
}

func moduleUnavailable(w http.ResponseWriter, name string) {
	apiError(w, 503, "module_unavailable", "Layanan "+name+" belum tersedia. Hubungi administrator.")
}

func array[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func (s *Server) apiRoutes(mux *http.ServeMux) {
	authenticated := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(fn)) }
	administrator := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, s.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.Admin == nil {
				moduleUnavailable(w, "administrasi")
				return
			}
			fn(w, r)
		})))
	}
	mux.HandleFunc("GET /api/auth/csrf", s.apiCSRF)
	mux.HandleFunc("POST /api/auth/login", s.apiLogin)
	authenticated("GET /api/auth/me", s.apiMe)
	authenticated("POST /api/auth/logout", s.apiLogout)
	authenticated("GET /api/capabilities", func(w http.ResponseWriter, r *http.Request) {
		apiData(w, map[string]bool{"workflow": s.Workflow != nil, "jobs": s.Jobs != nil, "leaders": s.Leaders != nil})
	})
	authenticated("GET /api/databases", s.apiDatabases)
	administrator("GET /api/admin/users", s.apiUsers)
	administrator("POST /api/admin/users", s.apiUserCreate)
	administrator("PATCH /api/admin/users/{uid}", s.apiUserUpdate)
	administrator("POST /api/admin/users/{uid}/password", s.apiUserPassword)
	administrator("GET /api/admin/groups", s.apiGroups)
	administrator("POST /api/admin/groups", s.apiGroupCreate)
	administrator("GET /api/admin/groups/{gid}/members", s.apiMembers)
	administrator("POST /api/admin/groups/{gid}/members", s.apiMemberAdd)
	administrator("DELETE /api/admin/groups/{gid}/members/{uid}", s.apiMemberRemove)
	administrator("PUT /api/admin/groups/{gid}/leader", s.apiLeader)
	administrator("GET /api/admin/connections", s.apiConnections)
	administrator("POST /api/admin/connections", s.apiConnectionSave)
	administrator("PUT /api/admin/connections/{cid}", s.apiConnectionSave)
	administrator("DELETE /api/admin/connections/{cid}", s.apiConnectionDelete)
	administrator("POST /api/admin/connections/test", s.apiConnectionTest)
	administrator("GET /api/admin/grants", s.apiGrants)
	administrator("PUT /api/admin/grants", s.apiGrantSave)
	administrator("DELETE /api/admin/grants/{gid}/{cid}", s.apiGrantRemove)
	authenticated("GET /api/db/{id}/schemas", s.apiSchemas)
	authenticated("GET /api/db/{id}/tables/{schema}/{table}", s.apiTable)
	authenticated("GET /api/db/{id}/data/{schema}/{table}", s.apiRows)
	authenticated("GET /api/db/{id}/diff/options", s.apiDiffOptions)
	authenticated("POST /api/db/{id}/diff", s.apiDiff)
	authenticated("POST /api/db/{id}/diff/drafts", s.apiDiff)
	authenticated("GET /api/db/{id}/migrations", s.apiMigrations)
	authenticated("GET /api/db/{id}/migrations/{mid}", s.apiMigration)
	authenticated("POST /api/db/{id}/migrations", s.apiDraft)
	authenticated("PUT /api/db/{id}/migrations/{mid}", s.apiDraft)
	authenticated("POST /api/db/{id}/migrations/{mid}/explain", s.apiExplain)
	authenticated("GET /api/db/{id}/history", s.apiHistory)
	authenticated("GET /api/db/{id}/requests", s.apiRequests)
	authenticated("POST /api/db/{id}/requests", s.apiPropose)
	authenticated("GET /api/db/{id}/requests/{rid}", s.apiRequest)
	authenticated("POST /api/db/{id}/requests/{rid}/{action}", s.apiRequestAction)
	authenticated("GET /api/db/{id}/jobs", s.apiJobs)
	authenticated("GET /api/db/{id}/jobs/{jid}", s.apiJob)
	authenticated("POST /api/db/{id}/jobs/{jid}/{action}", s.apiRecover)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 404, "not_found", "Endpoint tidak ditemukan.")
	})
}
