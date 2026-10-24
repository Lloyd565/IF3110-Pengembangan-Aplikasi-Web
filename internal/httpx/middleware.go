package httpx

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

type sessionContextKey struct{}

func currentSession(ctx context.Context) (auth.Session, bool) {
	s, ok := ctx.Value(sessionContextKey{}).(auth.Session)
	return s, ok
}

func (s *Server) sessionCookieName() string {
	if s.SecureCookies {
		return "__Host-session"
	}
	return "session"
}

func (s *Server) csrfCookieName() string {
	if s.SecureCookies {
		return "__Host-login_csrf"
	}
	return "login_csrf"
}

func (s *Server) secureCookie(r *http.Request) bool {
	return s.SecureCookies || r.TLS != nil || strings.HasPrefix(s.PublicOrigin, "https://")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, result auth.Resolution) {
	if result.Token == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookieName(), Value: result.Token, Path: "/", HttpOnly: true,
		Secure: s.secureCookie(r), SameSite: http.SameSiteLaxMode, Expires: result.Session.AbsoluteExpiresAt})
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", HttpOnly: true, Secure: s.secureCookie(r),
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}

func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		cookie, err := r.Cookie(s.sessionCookieName())
		if err == nil {
			if s.SessionManager == nil {
				s.renderError(w, r, http.StatusServiceUnavailable, "Layanan autentikasi belum tersedia.")
				return
			}
			result, err := s.SessionManager.Resolve(r.Context(), cookie.Value)
			if err != nil && !errors.Is(err, auth.ErrInvalidSession) {
				log.Print("session: backend unavailable")
				s.renderError(w, r, http.StatusServiceUnavailable, "Layanan autentikasi tidak dapat dihubungi. Coba lagi nanti.")
				return
			}
			if err == nil {
				s.setSessionCookie(w, r, result)
				ctx := reqctx.WithPrincipal(r.Context(), result.Session.Principal)
				ctx = reqctx.WithCSRFToken(ctx, result.Session.CSRFToken)
				ctx = context.WithValue(ctx, sessionContextKey{}, result.Session)
				r = r.WithContext(ctx)
			} else {
				s.clearCookie(w, r, s.sessionCookieName())
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request, publicOrigin string) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	} // Synchronizer/signed double-submit token is still mandatory.
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if publicOrigin != "" {
		public, publicErr := url.Parse(publicOrigin)
		if publicErr != nil || public.Host == "" {
			return false
		}
		scheme, host = public.Scheme, public.Host
	}
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, host) &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r, s.PublicOrigin) {
			s.renderError(w, r, http.StatusForbidden, "Asal request tidak diizinkan.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			status := http.StatusBadRequest
			if errors.As(err, new(*http.MaxBytesError)) {
				status = http.StatusRequestEntityTooLarge
			}
			s.renderError(w, r, status, "Form tidak valid atau terlalu besar.")
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.PostForm.Get("csrf_token")
		}
		want := reqctx.CSRFToken(r.Context())
		if _, loggedIn := reqctx.CurrentPrincipal(r.Context()); !loggedIn && r.URL.Path == "/login" {
			if cookie, err := r.Cookie(s.csrfCookieName()); err == nil && s.CSRF.ValidPreSession(cookie.Value) {
				want = cookie.Value
			}
		}
		if !auth.EqualCSRF(token, want) {
			s.renderError(w, r, http.StatusForbidden, "Token CSRF tidak valid. Muat ulang halaman lalu coba lagi.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := reqctx.CurrentPrincipal(r.Context()); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := reqctx.CurrentPrincipal(r.Context())
		if !p.IsPlatformAdmin {
			s.renderError(w, r, http.StatusForbidden, "Halaman ini hanya tersedia untuk Platform Admin.")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				// Panic values may contain secrets supplied to a downstream component.
				log.Print("http: request panic recovered")
				s.renderError(w, r, http.StatusInternalServerError, "Terjadi kesalahan pada server.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'none'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if s.SecureCookies {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStatus) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status := &responseStatus{ResponseWriter: w}
		completed := false
		defer func() {
			code := status.status
			if code == 0 {
				code = http.StatusOK
				if !completed {
					code = http.StatusInternalServerError
				}
			}
			// Do not log URL/path/query/body/headers: any can contain submitted secrets.
			log.Printf("http method=%q status=%d duration=%s", r.Method, code, time.Since(start).Round(time.Millisecond))
		}()
		next.ServeHTTP(status, r)
		completed = true
	})
}

type loginWindow struct {
	count int
	until time.Time
}
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]loginWindow
}

func (l *loginLimiter) allow(remote string, now time.Time) bool {
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		ip = remote
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, entry := range l.entries {
		if !now.Before(entry.until) {
			delete(l.entries, key)
		}
	}
	entry, exists := l.entries[ip]
	if !exists {
		if len(l.entries) >= 10000 {
			return false
		}
		entry.until = now.Add(time.Minute)
	}
	if entry.count >= 10 {
		return false
	}
	entry.count++
	l.entries[ip] = entry
	return true
}
