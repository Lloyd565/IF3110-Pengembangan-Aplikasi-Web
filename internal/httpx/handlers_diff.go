package httpx

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/explorer"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/schemadiff"
	"github.com/google/uuid"
)

type diffView struct {
	ConnID, ReferenceID                 uuid.UUID
	References                          []authz.ConnAccess
	ReferenceName                       string
	SourceSchemas, ReferenceSchemas     []explorer.Schema
	SourceSchema, ReferenceSchema, Name string
	SourceHash, ReferenceHash           string
	Acknowledged, CanCreate             bool
	Plan                                *schemadiff.Plan
	Error                               string
}

func (s *Server) diffView(w http.ResponseWriter, r *http.Request) (diffView, *dbconn.Target, *dbconn.Target, bool) {
	var v diffView
	conn, p, ok := s.dbAuthorize(w, r, authz.SchemaView)
	if !ok {
		return v, nil, nil, false
	}
	v.ConnID = conn
	get := r.URL.Query().Get
	if r.Method == http.MethodPost {
		get = r.PostForm.Get
	}
	v.SourceSchema, v.ReferenceSchema, v.Name = get("source_schema"), get("reference_schema"), get("name")
	v.Acknowledged = get("ack_destructive") == "1"
	if v.Name == "" {
		v.Name = "Schema diff"
	}
	v.ReferenceID = conn
	if value := get("reference_id"); value != "" {
		id, err := uuid.Parse(value)
		if err != nil {
			s.renderError(w, r, http.StatusUnprocessableEntity, "Koneksi referensi tidak valid.")
			return v, nil, nil, false
		}
		v.ReferenceID = id
	}
	if err := s.Authz.Can(r.Context(), p, v.ReferenceID, authz.SchemaView); err != nil {
		s.renderError(w, r, http.StatusForbidden, "Anda tidak memiliki akses schema pada koneksi referensi.")
		return v, nil, nil, false
	}
	v.CanCreate = s.can(r, p, conn, authz.MigrationCreate)
	var sourceKind, referenceKind dbconn.DBMS
	if err := s.DB.QueryRow(r.Context(), `SELECT a.dbms,b.dbms FROM db_connections a CROSS JOIN db_connections b
		WHERE a.id=$1 AND b.id=$2 AND a.deleted_at IS NULL AND b.deleted_at IS NULL`, conn, v.ReferenceID).Scan(&sourceKind, &referenceKind); err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Metadata koneksi tidak dapat dimuat.")
		return v, nil, nil, false
	}
	if sourceKind != referenceKind {
		s.renderError(w, r, http.StatusUnprocessableEntity, "Schema aktif dan referensi harus menggunakan DBMS yang sama.")
		return v, nil, nil, false
	}
	refs, err := s.Authz.VisibleConnections(r.Context(), p)
	if err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Daftar koneksi tidak dapat dimuat.")
		return v, nil, nil, false
	}
	v.References = refs
	for _, ref := range refs {
		if ref.ConnectionID == v.ReferenceID {
			v.ReferenceName = ref.Name
		}
	}
	source, err := s.Conns.Target(r.Context(), conn)
	if err != nil {
		s.renderError(w, r, http.StatusBadGateway, "Database aktif tidak dapat dihubungi.")
		return v, nil, nil, false
	}
	reference := source
	if v.ReferenceID != conn {
		reference, err = s.Conns.Target(r.Context(), v.ReferenceID)
		if err != nil {
			s.renderError(w, r, http.StatusBadGateway, "Database referensi tidak dapat dihubungi.")
			return v, nil, nil, false
		}
	}
	v.SourceSchemas, err = explorer.ListTargetSchemas(r.Context(), source)
	if err != nil {
		s.renderError(w, r, http.StatusBadGateway, "Schema aktif tidak dapat dimuat.")
		return v, nil, nil, false
	}
	v.ReferenceSchemas = v.SourceSchemas
	if v.ReferenceID != conn {
		v.ReferenceSchemas, err = explorer.ListTargetSchemas(r.Context(), reference)
		if err != nil {
			s.renderError(w, r, http.StatusBadGateway, "Schema referensi tidak dapat dimuat.")
			return v, nil, nil, false
		}
	}
	if r.Method == http.MethodGet {
		if !schemaListed(v.SourceSchemas, v.SourceSchema) && len(v.SourceSchemas) > 0 {
			v.SourceSchema = v.SourceSchemas[0].Name
		}
		if !schemaListed(v.ReferenceSchemas, v.ReferenceSchema) && len(v.ReferenceSchemas) > 0 {
			v.ReferenceSchema = v.ReferenceSchemas[0].Name
		}
	}
	return v, source, reference, true
}

func schemaListed(items []explorer.Schema, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return true
		}
	}
	return false
}
func (s *Server) schemaDiffForm(w http.ResponseWriter, r *http.Request) {
	v, _, _, ok := s.diffView(w, r)
	if ok {
		s.render(w, r, http.StatusOK, "db-schema-diff.html", v)
	}
}
func (s *Server) schemaDiffPreview(w http.ResponseWriter, r *http.Request) {
	s.schemaDiffSubmit(w, r, false)
}
func (s *Server) schemaDiffDraft(w http.ResponseWriter, r *http.Request) {
	s.schemaDiffSubmit(w, r, true)
}

func (s *Server) schemaDiffSubmit(w http.ResponseWriter, r *http.Request, save bool) {
	if save {
		if _, _, ok := s.dbAuthorize(w, r, authz.MigrationCreate); !ok {
			return
		}
	}
	v, source, reference, ok := s.diffView(w, r)
	if !ok {
		return
	}
	fail := func(status int, message string) { v.Error = message; s.render(w, r, status, "db-schema-diff.html", v) }
	if source.DBMS != reference.DBMS {
		fail(http.StatusUnprocessableEntity, "Schema aktif dan referensi harus menggunakan DBMS yang sama.")
		return
	}
	if !schemaListed(v.SourceSchemas, v.SourceSchema) || !schemaListed(v.ReferenceSchemas, v.ReferenceSchema) {
		fail(http.StatusUnprocessableEntity, "Pilih schema aktif dan referensi yang tersedia.")
		return
	}
	current, err := schemadiff.Read(r.Context(), source, v.SourceSchema)
	if err != nil {
		s.diffReadError(w, r, v, err)
		return
	}
	desired, err := schemadiff.Read(r.Context(), reference, v.ReferenceSchema)
	if err != nil {
		s.diffReadError(w, r, v, err)
		return
	}
	plan, err := schemadiff.Compare(current, desired)
	if err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	v.Plan = &plan
	v.SourceHash = current.Fingerprint()
	v.ReferenceHash = desired.Fingerprint()
	if !save {
		s.render(w, r, http.StatusOK, "db-schema-diff.html", v)
		return
	}
	if r.PostForm.Get("source_hash") == "" || r.PostForm.Get("reference_hash") == "" {
		fail(http.StatusUnprocessableEntity, "Tinjau preview perbandingan sebelum menyimpan draft.")
		return
	}
	if v.SourceHash != r.PostForm.Get("source_hash") || v.ReferenceHash != r.PostForm.Get("reference_hash") {
		fail(http.StatusConflict, "Schema berubah sejak preview. Tinjau ulang hasil perbandingan terbaru sebelum menyimpan.")
		return
	}
	if len(plan.Manual) > 0 {
		fail(http.StatusUnprocessableEntity, "Ada perubahan/dependensi yang memerlukan penanganan manual. Draft otomatis belum dapat dibuat.")
		return
	}
	if len(plan.Steps) == 0 {
		fail(http.StatusUnprocessableEntity, "Tidak ada SQL perubahan untuk disimpan.")
		return
	}
	if plan.Destructive && !v.Acknowledged {
		fail(http.StatusUnprocessableEntity, "Akui risiko perubahan yang menghapus data atau mengubah tipe sebelum menyimpan draft.")
		return
	}
	statements := []string{plan.SQL()}
	if source.DBMS == dbconn.MariaDB {
		statements = nil
		for _, step := range plan.Steps {
			statements = append(statements, step.SQL)
		}
	}
	p, _ := reqctx.CurrentPrincipal(r.Context())
	ids, err := migration.CreateDraftBatch(r.Context(), s.DB, v.ConnID, p.UserID, v.Name, statements)
	if err != nil {
		var validation migration.ValidationError
		if errors.As(err, &validation) {
			fail(http.StatusUnprocessableEntity, validation.Error())
		} else {
			log.Print("schema diff: draft batch could not be saved")
			fail(http.StatusInternalServerError, "Draft tidak dapat disimpan.")
		}
		return
	}
	if len(ids) == 1 {
		http.Redirect(w, r, fmt.Sprintf("/db/%s/migrations/%d/edit", v.ConnID, ids[0]), http.StatusSeeOther)
		return
	}
	var parts []string
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	http.Redirect(w, r, fmt.Sprintf("/db/%s/migrations?generated=%s", v.ConnID, strings.Join(parts, ",")), http.StatusSeeOther)
}

func (s *Server) diffReadError(w http.ResponseWriter, r *http.Request, v diffView, err error) {
	status := http.StatusBadGateway
	v.Error = "Snapshot schema tidak dapat dibaca. Coba ulangi perbandingan."
	if errors.Is(err, schemadiff.ErrNotFound) || errors.Is(err, schemadiff.ErrUnsupported) {
		status = http.StatusUnprocessableEntity
		v.Error = err.Error()
	} else {
		log.Print("schema diff: catalog snapshot unavailable")
	}
	s.render(w, r, status, "db-schema-diff.html", v)
}
