package httpx

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

func principalData(p reqctx.Principal) map[string]any {
	return map[string]any{"id": p.UserID, "username": p.Username, "is_platform_admin": p.IsPlatformAdmin}
}

func (s *Server) apiCSRF(w http.ResponseWriter, r *http.Request) {
	if _, ok := reqctx.CurrentPrincipal(r.Context()); ok {
		apiData(w, map[string]string{"csrf_token": reqctx.CSRFToken(r.Context())})
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
			apiFailure(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: s.csrfCookieName(), Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: int(auth.PreSessionTTL.Seconds())})
	apiData(w, map[string]string{"csrf_token": token})
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := reqctx.CurrentPrincipal(r.Context()); ok {
		s.apiMe(w, r)
		return
	}
	var form struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !apiDecode(w, r, &form) {
		return
	}
	if s.Credentials == nil || s.SessionManager == nil {
		moduleUnavailable(w, "autentikasi")
		return
	}
	if !s.loginLimits.allow(r.RemoteAddr, time.Now()) {
		w.Header().Set("Retry-After", "60")
		apiError(w, 429, "rate_limit", "Terlalu banyak percobaan login. Coba lagi dalam satu menit.")
		return
	}
	select {
	case s.passwordSlots <- struct{}{}:
		defer func() { <-s.passwordSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		apiError(w, 429, "busy", "Layanan login sedang sibuk.")
		return
	}
	if len(form.Username) > 200 || len(form.Password) > auth.MaxPasswordBytes {
		apiError(w, 401, "credentials", "Username atau password salah.")
		return
	}
	p, err := s.Credentials.Authenticate(r.Context(), strings.TrimSpace(form.Username), form.Password)
	if errors.Is(err, auth.ErrCredentials) {
		apiError(w, 401, "credentials", "Username atau password salah.")
		return
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	result, err := s.SessionManager.Issue(r.Context(), p)
	if err != nil {
		apiFailure(w, err)
		return
	}
	s.setSessionCookie(w, r, result)
	s.clearCookie(w, r, s.csrfCookieName())
	apiData(w, map[string]any{"user": principalData(p), "csrf_token": result.Session.CSRFToken})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	p, _ := reqctx.CurrentPrincipal(r.Context())
	apiData(w, map[string]any{"user": principalData(p), "csrf_token": reqctx.CSRFToken(r.Context())})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	session, ok := currentSession(r.Context())
	if !ok || s.SessionManager == nil {
		apiError(w, 401, "unauthenticated", "Silakan login kembali.")
		return
	}
	if err := s.SessionManager.Revoke(r.Context(), session.ID); err != nil {
		apiFailure(w, err)
		return
	}
	s.clearCookie(w, r, s.sessionCookieName())
	s.clearCookie(w, r, s.csrfCookieName())
	apiData(w, map[string]bool{"ok": true})
}
