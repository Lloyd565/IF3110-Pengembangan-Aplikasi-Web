package httpx

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

type loginView struct{ Username, Error string }

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := reqctx.CurrentPrincipal(r.Context()); ok {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	var token string
	if cookie, err := r.Cookie(s.csrfCookieName()); err == nil && s.CSRF.ValidPreSession(cookie.Value) {
		token = cookie.Value
	}
	if token == "" {
		var err error
		token, err = s.CSRF.NewPreSession()
		if err != nil {
			s.renderError(w, r, http.StatusServiceUnavailable, "Layanan autentikasi belum tersedia.")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: s.csrfCookieName(), Value: token, Path: "/", HttpOnly: true,
		Secure: s.secureCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: int(auth.PreSessionTTL.Seconds())})
	r = r.WithContext(reqctx.WithCSRFToken(r.Context(), token))
	s.render(w, r, http.StatusOK, "login.html", loginView{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if _, ok := reqctx.CurrentPrincipal(r.Context()); ok {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	if s.Credentials == nil || s.SessionManager == nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Layanan autentikasi belum tersedia.")
		return
	}
	if !s.loginLimits.allow(r.RemoteAddr, time.Now()) {
		w.Header().Set("Retry-After", "60")
		s.renderError(w, r, http.StatusTooManyRequests, "Terlalu banyak percobaan login. Coba lagi dalam satu menit.")
		return
	}
	select {
	case s.passwordSlots <- struct{}{}:
		defer func() { <-s.passwordSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		s.renderError(w, r, http.StatusTooManyRequests, "Layanan login sedang sibuk. Coba lagi sebentar.")
		return
	}
	username, password := strings.TrimSpace(r.PostForm.Get("username")), r.PostForm.Get("password")
	if len(username) > 200 || len(password) > auth.MaxPasswordBytes {
		s.renderLoginFailure(w, r, "", http.StatusUnauthorized)
		return
	}
	p, err := s.Credentials.Authenticate(r.Context(), username, password)
	if errors.Is(err, auth.ErrCredentials) {
		s.renderLoginFailure(w, r, username, http.StatusUnauthorized)
		return
	}
	if err != nil {
		log.Print("login: credential backend unavailable")
		s.renderError(w, r, http.StatusServiceUnavailable, "Layanan autentikasi tidak dapat dihubungi.")
		return
	}
	result, err := s.SessionManager.Issue(r.Context(), p)
	if err != nil {
		log.Print("login: session creation failed")
		s.renderError(w, r, http.StatusServiceUnavailable, "Tidak dapat membuat session. Silakan coba lagi.")
		return
	}
	s.setSessionCookie(w, r, result)
	s.clearCookie(w, r, s.csrfCookieName())
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) renderLoginFailure(w http.ResponseWriter, r *http.Request, username string, status int) {
	if cookie, err := r.Cookie(s.csrfCookieName()); err == nil {
		r = r.WithContext(reqctx.WithCSRFToken(r.Context(), cookie.Value))
	}
	s.render(w, r, status, "login.html", loginView{Username: username, Error: "Username atau password salah."})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	session, ok := currentSession(r.Context())
	if !ok || s.SessionManager == nil {
		s.renderError(w, r, http.StatusUnauthorized, "Session tidak valid.")
		return
	}
	if err := s.SessionManager.Revoke(r.Context(), session.ID); err != nil {
		log.Print("logout: revocation failed")
		s.renderError(w, r, http.StatusServiceUnavailable, "Session belum dapat diakhiri. Silakan coba lagi.")
		return
	}
	s.clearCookie(w, r, s.sessionCookieName())
	s.clearCookie(w, r, s.csrfCookieName())
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p, _ := reqctx.CurrentPrincipal(r.Context())
	connections, err := s.Authz.VisibleConnections(r.Context(), p)
	if err != nil {
		log.Print("dashboard: authorization backend unavailable")
		s.renderError(w, r, http.StatusServiceUnavailable, "Daftar database tidak dapat dimuat.")
		return
	}
	s.render(w, r, http.StatusOK, "dashboard.html", struct{ Connections []authz.ConnAccess }{connections})
}
