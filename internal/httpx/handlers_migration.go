package httpx

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

const maxFormBytes = 200 << 10

type editorView struct {
	ConnID     uuid.UUID
	Migration  *migration.Migration
	SourceID   int64
	Name       string
	SQL        string
	Error      string
	FormAction string
}

func migrationID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) migrationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, migration.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Migration tidak ditemukan.")
	case errors.Is(err, migration.ErrNotDraft):
		s.renderError(w, r, http.StatusConflict, "Migration ini sudah dieksekusi, jadi tidak bisa diubah atau dijalankan ulang. Buat draft baru dari migration tersebut.")
	default:
		log.Printf("migration %s: %v", r.URL.Path, err)
		s.renderError(w, r, http.StatusInternalServerError, "Terjadi kesalahan saat memproses migration.")
	}
}

func (s *Server) migrationList(w http.ResponseWriter, r *http.Request) {
	connID, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	list, err := migration.List(r.Context(), s.DB, connID)
	if err != nil {
		s.migrationError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "db-migrations.html", map[string]any{
		"ConnID":     connID,
		"Migrations": list,
		"CanCreate":  s.can(r, p, connID, authz.MigrationCreate),
	})
}

func (s *Server) migrationDetail(w http.ResponseWriter, r *http.Request) {
	connID, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	id, valid := migrationID(r)
	if !valid {
		s.migrationError(w, r, migration.ErrNotFound)
		return
	}
	s.renderDetail(w, r, p, connID, id, nil)
}

func (s *Server) renderDetail(w http.ResponseWriter, r *http.Request, p reqctx.Principal, connID uuid.UUID, id int64, extra map[string]any) {
	m, err := migration.Get(r.Context(), s.DB, connID, id)
	if err != nil {
		s.migrationError(w, r, err)
		return
	}
	data := map[string]any{
		"ConnID":     connID,
		"M":          m,
		"CanUpdate":  s.can(r, p, connID, authz.MigrationUpdate),
		"CanExecute": s.can(r, p, connID, authz.MigrationExecute),
		"CanCreate":  s.can(r, p, connID, authz.MigrationCreate),
	}
	for k, v := range extra {
		data[k] = v
	}
	s.render(w, r, http.StatusOK, "db-migration-detail.html", data)
}

func (s *Server) migrationExplain(w http.ResponseWriter, r *http.Request) {
	connID, target, ok := s.dbAccess(w, r, authz.MigrationExecute)
	if !ok {
		return
	}
	id, valid := migrationID(r)
	if !valid {
		s.migrationError(w, r, migration.ErrNotFound)
		return
	}
	p, _ := reqctx.CurrentPrincipal(r.Context())
	m, err := migration.Get(r.Context(), s.DB, connID, id)
	if err == nil && !m.IsDraft() {
		err = migration.ErrNotDraft
	}
	if err != nil {
		s.migrationError(w, r, err)
		return
	}
	plan, err := migration.Explain(r.Context(), target, m.SQL, r.PostFormValue("analyze") == "1")
	var ve migration.ValidationError
	switch {
	case err == nil:
		s.renderDetail(w, r, p, connID, id, map[string]any{"Plan": plan})
	case errors.As(err, &ve):
		s.renderDetail(w, r, p, connID, id, map[string]any{"PlanError": ve.Error()})
	case errors.Is(err, migration.ErrTargetUnavailable):
		log.Printf("explain %s/%d: %v", connID, id, err)
		s.renderError(w, r, http.StatusBadGateway, "Database target tidak dapat dihubungi.")
	default:
		s.migrationError(w, r, err)
	}
}

func (s *Server) migrationNewForm(w http.ResponseWriter, r *http.Request) {
	connID, _, ok := s.dbAuthorize(w, r, authz.MigrationCreate)
	if !ok {
		return
	}
	v := editorView{ConnID: connID, FormAction: fmt.Sprintf("/db/%s/migrations/new", connID)}
	if from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64); err == nil {
		src, err := migration.Get(r.Context(), s.DB, connID, from)
		if err != nil {
			s.migrationError(w, r, err)
			return
		}
		v.SourceID, v.Name, v.SQL = src.ID, src.Name, src.SQL
	}
	s.render(w, r, http.StatusOK, "db-migration-edit.html", v)
}

func (s *Server) migrationCreate(w http.ResponseWriter, r *http.Request) {
	connID, p, ok := s.dbAuthorize(w, r, authz.MigrationCreate)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	name, sql := r.PostFormValue("name"), r.PostFormValue("sql")
	id, err := migration.CreateDraft(r.Context(), s.DB, connID, p.UserID, name, sql)
	var ve migration.ValidationError
	if errors.As(err, &ve) {
		s.render(w, r, http.StatusUnprocessableEntity, "db-migration-edit.html", editorView{
			ConnID: connID, Name: name, SQL: sql, Error: ve.Error(),
			FormAction: fmt.Sprintf("/db/%s/migrations/new", connID),
		})
		return
	}
	if err != nil {
		s.migrationError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/db/%s/migrations/%d", connID, id), http.StatusSeeOther)
}

func (s *Server) migrationEditForm(w http.ResponseWriter, r *http.Request) {
	connID, _, ok := s.dbAuthorize(w, r, authz.MigrationUpdate)
	if !ok {
		return
	}
	id, valid := migrationID(r)
	if !valid {
		s.migrationError(w, r, migration.ErrNotFound)
		return
	}
	m, err := migration.Get(r.Context(), s.DB, connID, id)
	if err == nil && !m.IsDraft() {
		err = migration.ErrNotDraft
	}
	if err != nil {
		s.migrationError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "db-migration-edit.html", editorView{
		ConnID: connID, Migration: m, Name: m.Name, SQL: m.SQL,
		FormAction: fmt.Sprintf("/db/%s/migrations/%d/edit", connID, id),
	})
}

func (s *Server) migrationUpdate(w http.ResponseWriter, r *http.Request) {
	connID, _, ok := s.dbAuthorize(w, r, authz.MigrationUpdate)
	if !ok {
		return
	}
	id, valid := migrationID(r)
	if !valid {
		s.migrationError(w, r, migration.ErrNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	name, sql := r.PostFormValue("name"), r.PostFormValue("sql")
	err := migration.UpdateDraft(r.Context(), s.DB, connID, id, name, sql)
	var ve migration.ValidationError
	if errors.As(err, &ve) {
		m, getErr := migration.Get(r.Context(), s.DB, connID, id)
		if getErr != nil {
			s.migrationError(w, r, getErr)
			return
		}
		s.render(w, r, http.StatusUnprocessableEntity, "db-migration-edit.html", editorView{
			ConnID: connID, Migration: m, Name: name, SQL: sql, Error: ve.Error(),
			FormAction: fmt.Sprintf("/db/%s/migrations/%d/edit", connID, id),
		})
		return
	}
	if err != nil {
		s.migrationError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/db/%s/migrations/%d", connID, id), http.StatusSeeOther)
}

func (s *Server) migrationExecute(w http.ResponseWriter, r *http.Request) {
	connID, target, ok := s.dbAccess(w, r, authz.MigrationExecute)
	if !ok {
		return
	}
	id, valid := migrationID(r)
	if !valid {
		s.migrationError(w, r, migration.ErrNotFound)
		return
	}
	p, _ := reqctx.CurrentPrincipal(r.Context())
	err := migration.Execute(r.Context(), s.DB, target, connID, id, p.UserID)
	var ve migration.ValidationError
	switch {
	case err == nil:
		http.Redirect(w, r, fmt.Sprintf("/db/%s/migrations/%d", connID, id), http.StatusSeeOther)
	case errors.As(err, &ve):
		s.renderError(w, r, http.StatusUnprocessableEntity, ve.Error()+" Migration tetap berstatus draft.")
	case errors.Is(err, migration.ErrBusy):
		s.renderError(w, r, http.StatusConflict, "Masih ada migration lain yang sedang berjalan di database ini. Tunggu sampai selesai, lalu coba lagi.")
	case errors.Is(err, migration.ErrTargetUnavailable):
		log.Printf("execute %s/%d: %v", connID, id, err)
		s.renderError(w, r, http.StatusBadGateway, "Database target tidak dapat dihubungi. Migration tetap berstatus draft, silakan coba lagi nanti.")
	default:
		s.migrationError(w, r, err)
	}
}
