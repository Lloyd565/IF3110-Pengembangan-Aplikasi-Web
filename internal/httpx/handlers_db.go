package httpx

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/explorer"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

func (s *Server) dbAuthorize(w http.ResponseWriter, r *http.Request, act authz.Action) (connID uuid.UUID, p reqctx.Principal, ok bool) {
	p, loggedIn := reqctx.CurrentPrincipal(r.Context())
	if !loggedIn {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Database tidak ditemukan.")
		return
	}
	if err := s.Authz.Can(r.Context(), p, connID, act); err != nil {
		if !errors.Is(err, authz.ErrForbidden) {
			log.Printf("authz %s %s: %v", connID, act, err)
		}
		s.renderError(w, r, http.StatusForbidden, "Anda tidak punya akses untuk aksi ini pada database tersebut.")
		return
	}
	return connID, p, true
}

func (s *Server) can(r *http.Request, p reqctx.Principal, connID uuid.UUID, act authz.Action) bool {
	return s.Authz.Can(r.Context(), p, connID, act) == nil
}

func (s *Server) dbAccess(w http.ResponseWriter, r *http.Request, act authz.Action) (connID uuid.UUID, pool *dbconn.Target, ok bool) {
	connID, _, ok = s.dbAuthorize(w, r, act)
	if !ok {
		return
	}
	pool, err := s.Conns.Target(r.Context(), connID)
	if err != nil {
		log.Printf("pool %s: %v", connID, err)
		s.renderError(w, r, http.StatusBadGateway, "Database target tidak dapat dihubungi.")
		return
	}
	return connID, pool, true
}

func (s *Server) explorerError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, explorer.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Tabel tidak ditemukan.")
		return
	}
	log.Printf("explorer %s: %v", r.URL.Path, err)
	s.renderError(w, r, http.StatusBadGateway, "Gagal membaca database target. Coba lagi nanti.")
}

func (s *Server) dbSchema(w http.ResponseWriter, r *http.Request) {
	connID, pool, ok := s.dbAccess(w, r, authz.SchemaView)
	if !ok {
		return
	}
	schemas, err := explorer.ListTargetSchemas(r.Context(), pool)
	if err != nil {
		s.explorerError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "db-schema.html", map[string]any{
		"ConnID":  connID,
		"Schemas": schemas,
	})
}

func (s *Server) dbTable(w http.ResponseWriter, r *http.Request) {
	connID, pool, ok := s.dbAccess(w, r, authz.SchemaView)
	if !ok {
		return
	}
	table, err := explorer.DescribeTargetTable(r.Context(), pool, r.PathValue("schema"), r.PathValue("table"))
	if err != nil {
		s.explorerError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "db-table.html", map[string]any{
		"ConnID": connID,
		"Table":  table,
	})
}

func (s *Server) dbData(w http.ResponseWriter, r *http.Request) {
	connID, pool, ok := s.dbAccess(w, r, authz.DataView)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	schema, table := r.PathValue("schema"), r.PathValue("table")
	data, err := explorer.ReadTargetData(r.Context(), pool, schema, table, page)
	if err != nil {
		s.explorerError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "db-data.html", map[string]any{
		"ConnID":   connID,
		"Schema":   schema,
		"Table":    table,
		"Page":     data,
		"PrevPage": page - 1,
		"NextPage": page + 1,
	})
}
