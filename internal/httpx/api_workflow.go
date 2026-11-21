package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
)

func (s *Server) apiMigrations(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	if s.Workflow != nil {
		list, err := s.Workflow.List(r.Context(), p, id)
		if err != nil {
			apiFailure(w, err)
			return
		}
		apiData(w, array(list))
		return
	}
	list, err := migration.List(r.Context(), s.DB, id)
	if err != nil {
		apiFailure(w, err)
		return
	}
	result := []contracts.Migration{}
	for _, m := range list {
		result = append(result, legacyMigration(m))
	}
	apiData(w, result)
}

func (s *Server) apiMigration(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	if s.Workflow != nil {
		m, err := s.Workflow.Get(r.Context(), p, id, r.PathValue("mid"))
		if err != nil {
			apiFailure(w, err)
			return
		}
		apiData(w, m)
		return
	}
	mid, err := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	if err != nil || mid < 1 {
		apiFailure(w, migration.ErrNotFound)
		return
	}
	m, err := migration.Get(r.Context(), s.DB, id, mid)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, legacyMigration(*m))
}

func (s *Server) apiDraft(w http.ResponseWriter, r *http.Request) {
	action := authz.MigrationCreate
	if r.Method == http.MethodPut {
		action = authz.MigrationUpdate
	}
	id, p, ok := s.dbAuthorize(w, r, action)
	if !ok {
		return
	}
	var draft contracts.Draft
	if !apiDecode(w, r, &draft) {
		return
	}
	if strings.TrimSpace(draft.Name) == "" || utf8.RuneCountInString(draft.Name) > 200 || strings.TrimSpace(draft.SQLUp) == "" || len(draft.SQLUp) > 100000 || len(draft.SQLDown) > 100000 {
		apiError(w, 422, "validation", "Nama dan SQL up wajib diisi; nama maksimal 200 karakter dan setiap SQL maksimal 100 KB.")
		return
	}
	if draft.Irreversible {
		if strings.TrimSpace(draft.IrreversibleReason) == "" || strings.TrimSpace(draft.SQLDown) != "" {
			apiError(w, 422, "validation", "Migration irreversible memerlukan alasan dan SQL down kosong.")
			return
		}
	} else if strings.TrimSpace(draft.SQLDown) == "" {
		apiError(w, 422, "validation", "Isi SQL down atau tandai irreversible beserta alasannya.")
		return
	}
	if s.Workflow == nil {
		moduleUnavailable(w, "workflow migration")
		return
	}
	m, err := s.Workflow.Save(r.Context(), p, id, r.PathValue("mid"), draft)
	if err != nil {
		apiFailure(w, err)
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	apiJSON(w, status, map[string]any{"data": m})
}

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	if s.Workflow == nil {
		moduleUnavailable(w, "execution history")
		return
	}
	list, err := s.Workflow.History(r.Context(), p, id, r.URL.Query().Get("hide_rollback") == "true")
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, array(list))
}

func (s *Server) apiRequests(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	if s.Workflow == nil {
		moduleUnavailable(w, "review dan approval")
		return
	}
	list, err := s.Workflow.Requests(r.Context(), p, id)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, array(list))
}

func (s *Server) apiRequest(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	if s.Workflow == nil {
		moduleUnavailable(w, "review dan approval")
		return
	}
	x, err := s.Workflow.Request(r.Context(), p, id, r.PathValue("rid"))
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, x)
}

func (s *Server) apiPropose(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationCreate)
	if !ok {
		return
	}
	var f contracts.ChangeInput
	if !apiDecode(w, r, &f) {
		return
	}
	if f.MigrationID == "" || (f.Action != "apply" && f.Action != "rollback" && f.Action != "forward") || (f.Mode != "normal" && f.Mode != "batch") || (f.Mode == "batch" && (f.BatchSize < 1 || f.BatchSize > 100000 || strings.TrimSpace(f.CursorColumn) == "")) {
		apiError(w, 422, "validation", "Periksa migration, aksi, mode, ukuran batch, dan kolom cursor.")
		return
	}
	if s.Workflow == nil {
		moduleUnavailable(w, "workflow migration")
		return
	}
	x, err := s.Workflow.Propose(r.Context(), p, id, f)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiJSON(w, 201, map[string]any{"data": x})
}

func idempotency(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		apiError(w, 422, "idempotency_key", "Idempotency-Key sepanjang 8 sampai 128 karakter wajib diisi.")
		return "", false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			apiError(w, 422, "idempotency_key", "Idempotency-Key hanya boleh berisi huruf, angka, tanda minus, atau underscore.")
			return "", false
		}
	}
	return key, true
}

func (s *Server) apiRequestAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	permission := authz.MigrationExecute
	if action == "review" {
		permission = authz.MigrationView
	}
	id, p, ok := s.dbAuthorize(w, r, permission)
	if !ok {
		return
	}
	if s.Workflow == nil {
		moduleUnavailable(w, "workflow migration")
		return
	}
	switch action {
	case "submit":
		x, err := s.Workflow.Submit(r.Context(), p, id, r.PathValue("rid"))
		if err != nil {
			apiFailure(w, err)
			return
		}
		apiData(w, x)
	case "review":
		var f contracts.Review
		if !apiDecode(w, r, &f) {
			return
		}
		if (f.Decision != "approve" && f.Decision != "reject") || utf8.RuneCountInString(f.Comment) > 4000 {
			apiError(w, 422, "validation", "Keputusan review atau komentar tidak valid.")
			return
		}
		x, err := s.Workflow.Review(r.Context(), p, id, r.PathValue("rid"), f)
		if err != nil {
			apiFailure(w, err)
			return
		}
		apiData(w, x)
	case "enqueue":
		key, ok := idempotency(w, r)
		if !ok {
			return
		}
		job, err := s.Workflow.Enqueue(r.Context(), p, id, r.PathValue("rid"), key)
		if err != nil {
			apiFailure(w, err)
			return
		}
		apiJSON(w, 202, map[string]any{"data": job})
	default:
		apiError(w, 404, "not_found", "Aksi tidak ditemukan.")
	}
}

func (s *Server) apiJobs(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	if s.Jobs == nil {
		moduleUnavailable(w, "queue dan worker")
		return
	}
	list, err := s.Jobs.List(r.Context(), p, id)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, array(list))
}

func (s *Server) apiJob(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationView)
	if !ok {
		return
	}
	if s.Jobs == nil {
		moduleUnavailable(w, "queue dan worker")
		return
	}
	job, err := s.Jobs.Get(r.Context(), p, id, r.PathValue("jid"))
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiData(w, job)
}

func (s *Server) apiRecover(w http.ResponseWriter, r *http.Request) {
	id, p, ok := s.dbAuthorize(w, r, authz.MigrationExecute)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "resume" && action != "compensate" {
		apiError(w, 404, "not_found", "Aksi tidak ditemukan.")
		return
	}
	key, ok := idempotency(w, r)
	if !ok {
		return
	}
	if s.Jobs == nil {
		moduleUnavailable(w, "pemulihan batch")
		return
	}
	request, err := s.Jobs.Recover(r.Context(), p, id, r.PathValue("jid"), action, key)
	if err != nil {
		apiFailure(w, err)
		return
	}
	apiJSON(w, 201, map[string]any{"data": request})
}
