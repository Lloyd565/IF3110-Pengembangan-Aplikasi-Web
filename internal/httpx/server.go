package httpx

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/admin"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/web"
)

type Server struct {
	DB       *pgxpool.Pool
	Sessions *auth.Sessions
	Authz    *authz.Authorizer
	Conns    *dbconn.Registry
	Admin    *admin.Service
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/", http.FileServerFS(web.FS))

	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /dashboard", s.dashboard)

	mux.HandleFunc("/admin/users", s.adminUsers)
	mux.HandleFunc("/admin/groups", s.adminGroups)
	mux.HandleFunc("/admin/connections", s.adminConnections)
	mux.HandleFunc("/admin/grants", s.adminGrants)

	mux.HandleFunc("GET /db/{id}/schema", s.dbSchema)
	mux.HandleFunc("GET /db/{id}/tables/{schema}/{table}", s.dbTable)
	mux.HandleFunc("GET /db/{id}/data/{schema}/{table}", s.dbData)
	mux.HandleFunc("GET /db/{id}/migrations", s.migrationList)
	mux.HandleFunc("GET /db/{id}/migrations/new", s.migrationNewForm)
	mux.HandleFunc("POST /db/{id}/migrations/new", s.migrationCreate)
	mux.HandleFunc("GET /db/{id}/migrations/{mid}", s.migrationDetail)
	mux.HandleFunc("GET /db/{id}/migrations/{mid}/edit", s.migrationEditForm)
	mux.HandleFunc("POST /db/{id}/migrations/{mid}/edit", s.migrationUpdate)
	mux.HandleFunc("POST /db/{id}/migrations/{mid}/execute", s.migrationExecute)

	return devPrincipal(mux)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		log.Printf("healthz: %v", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "belum diimplementasi", http.StatusNotImplemented)
}
