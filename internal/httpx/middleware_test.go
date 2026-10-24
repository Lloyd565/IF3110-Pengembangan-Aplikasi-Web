package httpx

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

func TestCSRFAndOriginMiddleware(t *testing.T) {
	s := &Server{CSRF: auth.CSRF{Key: make([]byte, 32)}}
	pre, err := s.CSRF.NewPreSession()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session valid", "missing", "wrong", "query only", "login valid", "login forged", "cross origin", "null origin", "cross site", "proxy origin", "put", "delete", "head", "oversize"} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{"csrf_token": {"session-csrf"}}
			method, path, expected := "POST", "/logout", 204
			loggedIn := true
			s.PublicOrigin = ""
			switch name {
			case "missing":
				form.Del("csrf_token")
				expected = 403
			case "wrong":
				form.Set("csrf_token", "wrong")
				expected = 403
			case "query only":
				form.Del("csrf_token")
				path += "?csrf_token=session-csrf"
				expected = 403
			case "login valid", "login forged":
				loggedIn = false
				path = "/login"
				form.Set("csrf_token", pre)
			case "cross origin", "null origin", "cross site":
				expected = 403
			case "proxy origin":
				s.PublicOrigin = "https://db.example.test"
			case "put":
				method = "PUT"
			case "delete":
				method = "DELETE"
			case "head":
				method = "HEAD"
				form.Del("csrf_token")
			case "oversize":
				form.Set("payload", strings.Repeat("x", maxFormBytes+1))
				expected = 413
			}
			r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if method == "DELETE" {
				r.Header.Set("X-CSRF-Token", form.Get("csrf_token"))
			}
			if loggedIn {
				ctx := reqctx.WithPrincipal(r.Context(), reqctx.Principal{UserID: 1})
				r = r.WithContext(reqctx.WithCSRFToken(ctx, "session-csrf"))
			} else {
				cookie := pre
				if name == "login forged" {
					cookie += "forged"
					form.Set("csrf_token", cookie)
					expected = 403
				}
				r.AddCookie(&http.Cookie{Name: s.csrfCookieName(), Value: cookie})
			}
			switch name {
			case "cross origin":
				r.Header.Set("Origin", "https://evil.test")
			case "null origin":
				r.Header.Set("Origin", "null")
			case "cross site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "proxy origin":
				r.Header.Set("Origin", s.PublicOrigin)
			}
			w := httptest.NewRecorder()
			s.csrfMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
			if w.Code != expected {
				t.Fatalf("status = %d, want %d", w.Code, expected)
			}
		})
	}
}

func TestRequireAuthAndAdmin(t *testing.T) {
	s := &Server{}
	for _, test := range []struct {
		principal *reqctx.Principal
		status    int
	}{
		{nil, 303}, {&reqctx.Principal{UserID: 1}, 403}, {&reqctx.Principal{UserID: 2, IsPlatformAdmin: true}, 204},
	} {
		r := httptest.NewRequest("GET", "/admin/users", nil)
		if test.principal != nil {
			r = r.WithContext(reqctx.WithPrincipal(context.Background(), *test.principal))
		}
		w := httptest.NewRecorder()
		s.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("admin status = %d, want %d", w.Code, test.status)
		}
	}
}

func TestSecurityHeadersRecoveryAndSecretFreeLogging(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	s := &Server{SecureCookies: true}
	handler := s.recoverMiddleware(s.secureHeaders(requestLogging(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret-panic-value") }))))
	r := httptest.NewRequest("POST", "https://example.test/secret-path?password=secret-query", strings.NewReader("password=secret-body"))
	r.Header.Set("Cookie", "session=secret-cookie")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 500 || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("recovery/headers")
	}
	if strings.Contains(logs.String(), "secret-") || strings.Contains(w.Body.String(), "secret-") {
		t.Fatal("secret leaked")
	}
	if !strings.Contains(logs.String(), "status=500") {
		t.Fatal("panic logged with wrong status")
	}
}

func TestLoginLimiter(t *testing.T) {
	l := &loginLimiter{entries: make(map[string]loginWindow)}
	now := time.Now()
	for range 10 {
		if !l.allow("127.0.0.1:1234", now) {
			t.Fatal("early rate limit")
		}
	}
	if l.allow("127.0.0.1:5678", now) {
		t.Fatal("port changed to bypass rate limit")
	}
	if !l.allow("127.0.0.2:1234", now) || !l.allow("127.0.0.1:1234", now.Add(time.Minute)) {
		t.Fatal("rate window did not reset")
	}
}

func TestSessionCookieTransportFlags(t *testing.T) {
	for _, tc := range []struct {
		name, requestURL, publicOrigin string
		production, secure             bool
	}{
		{"development HTTP", "http://example.test", "", false, false},
		{"development HTTPS", "https://example.test", "", false, true},
		{"production behind proxy", "http://example.test", "", true, true},
		{"HTTPS origin behind proxy", "http://internal.test", "https://example.test", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{SecureCookies: tc.production, PublicOrigin: tc.publicOrigin}
			r := httptest.NewRequest("GET", tc.requestURL, nil)
			w := httptest.NewRecorder()
			s.setSessionCookie(w, r, auth.Resolution{Token: "opaque", Session: auth.Session{AbsoluteExpiresAt: time.Now().Add(time.Hour)}})
			s.clearCookie(w, r, s.csrfCookieName())
			for _, cookie := range w.Result().Cookies() {
				if cookie.Secure != tc.secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
					t.Fatal("cookie transport flags")
				}
			}
		})
	}
}
