package httpx

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/explorer"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/schemadiff"
)

func (s *Server) apiDatabases(w http.ResponseWriter, r *http.Request) {
	list, err := s.Authz.VisibleConnections(r.Context(), actor(r))
	if err != nil {
		apiFailure(w, err)
		return
	}
	result := []map[string]any{}
	for _, c := range list {
		var kind, database string
		if err := s.DB.QueryRow(r.Context(), "SELECT dbms,dbname FROM db_connections WHERE id=$1 AND deleted_at IS NULL", c.ConnectionID).Scan(&kind, &database); err != nil {
			apiFailure(w, err)
			return
		}
		result = append(result, map[string]any{"id": c.ConnectionID, "name": c.Name, "level": c.Level, "groups": array(c.Groups), "dbms": kind, "database": database})
	}
	apiData(w, result)
}

func (s *Server) apiSchemas(w http.ResponseWriter, r *http.Request) {
	_, t, ok := s.dbAccess(w, r, authz.SchemaView)
	if !ok {
		return
	}
	schemas, err := explorer.ListTargetSchemas(r.Context(), t)
	if err != nil {
		apiFailure(w, err)
		return
	}
	result := []map[string]any{}
	for _, schema := range schemas {
		tables := []map[string]string{}
		for _, t := range schema.Tables {
			tables = append(tables, map[string]string{"name": t.Name, "kind": t.Kind})
		}
		result = append(result, map[string]any{"name": schema.Name, "tables": tables})
	}
	apiData(w, result)
}

func (s *Server) apiTable(w http.ResponseWriter, r *http.Request) {
	_, t, ok := s.dbAccess(w, r, authz.SchemaView)
	if !ok {
		return
	}
	detail, err := explorer.DescribeTargetTable(r.Context(), t, r.PathValue("schema"), r.PathValue("table"))
	if err != nil {
		apiFailure(w, err)
		return
	}
	columns := []map[string]any{}
	for _, c := range detail.Columns {
		columns = append(columns, map[string]any{"name": c.Name, "type": c.Type, "nullable": c.Nullable, "default": c.Default})
	}
	constraints := []map[string]string{}
	for _, c := range detail.Constraints {
		constraints = append(constraints, map[string]string{"name": c.Name, "type": c.Type, "definition": c.Definition})
	}
	indexes := []map[string]string{}
	for _, i := range detail.Indexes {
		indexes = append(indexes, map[string]string{"name": i.Name, "definition": i.Definition})
	}
	apiData(w, map[string]any{"schema": detail.Schema, "name": detail.Name, "kind": detail.Kind, "columns": columns, "constraints": constraints, "indexes": indexes})
}

func (s *Server) apiRows(w http.ResponseWriter, r *http.Request) {
	_, t, ok := s.dbAccess(w, r, authz.DataView)
	if !ok {
		return
	}
	page := 1
	if q := r.URL.Query().Get("page"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > 1000000 {
			apiError(w, 422, "validation", "Nomor halaman harus antara 1 dan 1000000.")
			return
		}
		page = n
	}
	d, err := explorer.ReadTargetData(r.Context(), t, r.PathValue("schema"), r.PathValue("table"), page)
	if err != nil {
		apiFailure(w, err)
		return
	}
	rows := [][]any{}
	for _, row := range d.Rows {
		values := []any{}
		for _, c := range row {
			if c.Null {
				values = append(values, nil)
			} else {
				values = append(values, c.Value)
			}
		}
		rows = append(rows, values)
	}
	apiData(w, map[string]any{"columns": array(d.Columns), "rows": rows, "page": d.Page, "has_next": d.HasNext, "page_size": explorer.PageSize})
}

func (s *Server) apiDiffOptions(w http.ResponseWriter, r *http.Request) {
	v, source, _, ok := s.diffView(w, r)
	if !ok {
		return
	}
	refs := []map[string]any{}
	for _, ref := range v.References {
		var kind dbconn.DBMS
		if err := s.DB.QueryRow(r.Context(), "SELECT dbms FROM db_connections WHERE id=$1", ref.ConnectionID).Scan(&kind); err != nil {
			apiFailure(w, err)
			return
		}
		if kind == source.DBMS {
			refs = append(refs, map[string]any{"id": ref.ConnectionID, "name": ref.Name})
		}
	}
	sourceSchemas := []string{}
	for _, x := range v.SourceSchemas {
		sourceSchemas = append(sourceSchemas, x.Name)
	}
	referenceSchemas := []string{}
	for _, x := range v.ReferenceSchemas {
		referenceSchemas = append(referenceSchemas, x.Name)
	}
	apiData(w, map[string]any{"references": refs, "source_schemas": sourceSchemas, "reference_schemas": referenceSchemas, "source_schema": v.SourceSchema, "reference_schema": v.ReferenceSchema, "reference_id": v.ReferenceID, "can_create": v.CanCreate, "dbms": source.DBMS})
}

func (s *Server) apiDiff(w http.ResponseWriter, r *http.Request) {
	save := strings.HasSuffix(r.URL.Path, "/drafts")
	if save {
		if _, _, ok := s.dbAuthorize(w, r, authz.MigrationCreate); !ok {
			return
		}
	}
	var f struct {
		ReferenceID     string `json:"reference_id"`
		SourceSchema    string `json:"source_schema"`
		ReferenceSchema string `json:"reference_schema"`
		SourceHash      string `json:"source_hash"`
		ReferenceHash   string `json:"reference_hash"`
		Name            string `json:"name"`
		Acknowledged    bool   `json:"ack_destructive"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	r.PostForm = url.Values{"reference_id": {f.ReferenceID}, "source_schema": {f.SourceSchema}, "reference_schema": {f.ReferenceSchema}}
	v, source, reference, ok := s.diffView(w, r)
	if !ok {
		return
	}
	if !schemaListed(v.SourceSchemas, f.SourceSchema) || !schemaListed(v.ReferenceSchemas, f.ReferenceSchema) {
		apiError(w, 422, "validation", "Pilih schema aktif dan referensi yang tersedia.")
		return
	}
	current, err := schemadiff.Read(r.Context(), source, f.SourceSchema)
	if err != nil {
		apiFailure(w, err)
		return
	}
	desired, err := schemadiff.Read(r.Context(), reference, f.ReferenceSchema)
	if err != nil {
		apiFailure(w, err)
		return
	}
	plan, err := schemadiff.Compare(current, desired)
	if err != nil {
		apiError(w, 422, "validation", "Perbandingan schema tidak dapat dibuat.")
		return
	}
	if !save {
		changes := []map[string]any{}
		for _, c := range plan.Changes {
			changes = append(changes, map[string]any{"object": c.Object, "description": c.Description, "destructive": c.Destructive})
		}
		steps := []map[string]string{}
		for _, x := range plan.Steps {
			steps = append(steps, map[string]string{"description": x.Description, "sql": x.SQL})
		}
		apiData(w, map[string]any{"changes": changes, "steps": steps, "sql": plan.SQL(), "manual": array(plan.Manual), "warnings": array(plan.Warnings), "destructive": plan.Destructive, "source_hash": current.Fingerprint(), "reference_hash": desired.Fingerprint()})
		return
	}
	if f.SourceHash == "" || f.ReferenceHash == "" {
		apiError(w, 422, "preview_required", "Tinjau preview sebelum menyimpan draft.")
		return
	}
	if f.SourceHash != current.Fingerprint() || f.ReferenceHash != desired.Fingerprint() {
		apiError(w, 409, "schema_drift", "Schema berubah sejak preview. Bandingkan kembali.")
		return
	}
	if len(plan.Manual) > 0 || len(plan.Steps) == 0 {
		apiError(w, 422, "manual_required", "Tidak ada SQL otomatis yang dapat disimpan; periksa penanganan manual.")
		return
	}
	if plan.Destructive && !f.Acknowledged {
		apiError(w, 422, "acknowledgement", "Akui risiko perubahan sebelum menyimpan.")
		return
	}
	if strings.TrimSpace(f.Name) == "" || len([]rune(f.Name)) > 180 {
		apiError(w, 422, "validation", "Nama migration wajib diisi, maksimal 180 karakter.")
		return
	}
	if s.Workflow == nil {
		moduleUnavailable(w, "workflow migration")
		return
	}
	statements := []string{plan.SQL()}
	if source.DBMS == dbconn.MariaDB {
		statements = nil
		for _, x := range plan.Steps {
			statements = append(statements, x.SQL)
		}
	}
	created, err := s.Workflow.SaveGenerated(r.Context(), actor(r), v.ConnID, f.Name, statements)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiJSON(w, 201, map[string]any{"data": array(created)})
}

func (s *Server) apiExplain(w http.ResponseWriter, r *http.Request) {
	_, target, ok := s.dbAccess(w, r, authz.MigrationExecute)
	if !ok {
		return
	}
	var f struct {
		SQL     string `json:"sql"`
		Analyze bool   `json:"analyze"`
	}
	if !apiDecode(w, r, &f) {
		return
	}
	if strings.TrimSpace(f.SQL) == "" || len(f.SQL) > 100000 {
		apiError(w, 422, "validation", "SQL wajib diisi, maksimal 100 KB.")
		return
	}
	plan, err := migration.ExplainTarget(r.Context(), target, f.SQL, f.Analyze)
	if err != nil {
		var v migration.ValidationError
		if errors.As(err, &v) {
			apiError(w, 422, "validation", string(v))
		} else {
			apiFailure(w, err)
		}
		return
	}
	apiData(w, plan)
}

func legacyMigration(m migration.Migration) contracts.Migration {
	errMessage := ""
	if m.ErrorMessage != nil {
		errMessage = *m.ErrorMessage
	}
	return contracts.Migration{Draft: contracts.Draft{Name: m.Name, SQLUp: m.SQL, Irreversible: true, IrreversibleReason: "Legacy M1: SQL down belum tersedia."}, ID: strconv.FormatInt(m.ID, 10), DatabaseID: m.ConnectionID.String(), Version: m.Version, DBMS: string(m.DBMS), Status: m.Status, CreatedAt: m.CreatedAt, CreatedBy: m.CreatedByName, ErrorMessage: errMessage, CanEdit: false}
}
