package httpx

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/web"
)

type Authenticator interface {
	Authenticate(context.Context, string, string) (reqctx.Principal, error)
}

type Server struct {
	DB             *pgxpool.Pool
	Sessions       *auth.Sessions
	Authz          *authz.Authorizer
	Conns          *dbconn.Registry
	Credentials    Authenticator
	SessionManager *auth.Manager
	CSRF           auth.CSRF
	SecureCookies  bool
	PublicOrigin   string
	loginLimits    *loginLimiter
	passwordSlots  chan struct{}
}

func (s *Server) ConfigureAuth(policy auth.Policy, key []byte, secure bool) error {
	if s.Sessions == nil {
		s.Sessions = &auth.Sessions{DB: s.DB}
	}
	manager, err := auth.NewManager(s.Sessions, policy, key)
	if err != nil {
		return err
	}
	credentials, err := auth.NewCredentials(s.DB)
	if err != nil {
		return err
	}
	s.SessionManager, s.Credentials = manager, credentials
	s.CSRF, s.SecureCookies = auth.CSRF{Key: append([]byte(nil), key...)}, secure
	return nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	s.loginLimits = &loginLimiter{entries: make(map[string]loginWindow)}
	s.passwordSlots = make(chan struct{}, 4)
	protected := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, s.requireAuth(handler))
	}
	admin := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, s.requireAdmin(handler))
	}

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/", http.FileServerFS(web.FS))

	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	protected("POST /logout", s.logout)
	protected("GET /dashboard", s.dashboard)

	admin("/admin/users", s.adminUsers)
	admin("/admin/groups", s.adminGroups)
	admin("/admin/connections", s.adminConnections)
	admin("/admin/grants", s.adminGrants)

	protected("GET /db/{id}/schema", s.dbSchema)
	protected("GET /db/{id}/tables/{schema}/{table}", s.dbTable)
	protected("GET /db/{id}/data/{schema}/{table}", s.dbData)
	protected("GET /db/{id}/migrations", s.migrationList)
	protected("GET /db/{id}/migrations/new", s.migrationNewForm)
	protected("POST /db/{id}/migrations/new", s.migrationCreate)
	protected("GET /db/{id}/migrations/{mid}", s.migrationDetail)
	protected("GET /db/{id}/migrations/{mid}/edit", s.migrationEditForm)
	protected("POST /db/{id}/migrations/{mid}/edit", s.migrationUpdate)
	protected("POST /db/{id}/migrations/{mid}/execute", s.migrationExecute)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusNotFound, "Halaman tidak ditemukan.")
	})

	return s.recoverMiddleware(s.secureHeaders(requestLogging(s.sessionMiddleware(s.csrfMiddleware(mux)))))
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
