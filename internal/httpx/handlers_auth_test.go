package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

var csrfInput = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func TestIntegrationLoginAccessLogout(t *testing.T) {
	db := testdb.Fresh(t, "auth_http")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword("test-password-rahasia")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (id, username, password_hash) VALUES (1, 'budi', $1)`, hash); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db, Authz: &authz.Authorizer{DB: db}}
	if err := s.ConfigureAuth(auth.DefaultPolicy(), make([]byte, 32), true); err != nil {
		t.Fatal(err)
	}
	h := s.Routes()
	t.Setenv("DEV_USER_ID", "1") // The former development bypass must have no effect.
	request := func(method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://example.test"+path, strings.NewReader(form.Encode()))
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	findCookie := func(w *httptest.ResponseRecorder, name string) *http.Cookie {
		t.Helper()
		for _, c := range w.Result().Cookies() {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("cookie %s missing", name)
		return nil
	}
	csrfToken := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		matches := csrfInput.FindStringSubmatch(w.Body.String())
		if len(matches) != 2 {
			t.Fatal("CSRF input missing")
		}
		return matches[1]
	}
	for _, path := range []string{"/dashboard", "/admin/users", "/db/anything/schema"} {
		w := request("GET", path, nil)
		if w.Code != 303 || w.Header().Get("Location") != "/login" {
			t.Fatalf("unprotected route %s: %d", path, w.Code)
		}
	}
	w := request("GET", "/login", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("login form")
	}
	pre, token := findCookie(w, s.csrfCookieName()), csrfToken(w)
	form := url.Values{"username": {"budi"}, "password": {"test-password-rahasia"}, "csrf_token": {token}}
	if request("POST", "/login", form).Code != 403 {
		t.Fatal("login without CSRF cookie accepted")
	}
	form.Set("password", "wrong-password")
	w = request("POST", "/login", form, pre)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "Username atau password salah.") || strings.Contains(w.Body.String(), "wrong-password") {
		t.Fatal("login failure leaked credentials")
	}
	form.Set("password", "test-password-rahasia")
	w = request("POST", "/login", form, pre, &http.Cookie{Name: s.sessionCookieName(), Value: "fixed-invalid-id"})
	if w.Code != 303 || w.Header().Get("Location") != "/dashboard" {
		t.Fatalf("login: %d", w.Code)
	}
	cookie := findCookie(w, s.sessionCookieName())
	// Ignore the earlier deletion cookie when an invalid incoming ID was cleared.
	for _, c := range w.Result().Cookies() {
		if c.Name == s.sessionCookieName() && c.Value != "" {
			cookie = c
		}
	}
	if !auth.ValidToken(cookie.Value) || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatal("session cookie flags")
	}
	if strings.Contains(w.Body.String(), cookie.Value) {
		t.Fatal("session identifier leaked in response body")
	}
	w = request("GET", "/dashboard", nil, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "budi") {
		t.Fatal("authenticated dashboard")
	}
	sessionCSRF := csrfToken(w)
	if request("GET", "/admin/users", nil, cookie).Code != 403 {
		t.Fatal("ordinary user reached admin")
	}
	if request("GET", "/db/aaaaaaaa-0000-0000-0000-000000000001/schema", nil, cookie).Code != 403 {
		t.Fatal("authorization scaffold must fail closed")
	}
	if request("POST", "/logout", nil, cookie).Code != 403 {
		t.Fatal("logout without CSRF accepted")
	}
	if request("POST", "/logout", url.Values{"csrf_token": {"wrong"}}, cookie).Code != 403 {
		t.Fatal("logout wrong CSRF accepted")
	}
	if _, err := db.Exec(ctx, `UPDATE users SET is_platform_admin = true WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if request("GET", "/admin/users", nil, cookie).Code != 501 {
		t.Fatal("admin principal not refreshed")
	}
	if err := s.Sessions.MarkForceRotate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/dashboard", nil, cookie)
	rotated := findCookie(w, s.sessionCookieName())
	if rotated.Value == cookie.Value || !rotated.Expires.Equal(cookie.Expires) {
		t.Fatal("rotation/absolute cookie contract")
	}
	w = request("POST", "/logout", url.Values{"csrf_token": {sessionCSRF}}, rotated)
	if w.Code != 303 || findCookie(w, s.sessionCookieName()).MaxAge >= 0 {
		t.Fatal("logout cookie not cleared")
	}
	for _, replay := range []*http.Cookie{cookie, rotated} {
		if request("GET", "/dashboard", nil, replay).Code != 303 {
			t.Fatal("revoked session accepted")
		}
	}
	var revoked *time.Time
	if err := db.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE user_id = 1`).Scan(&revoked); err != nil || revoked == nil {
		t.Fatal("logout did not revoke backend session", err)
	}
	db.Close()
	if request("GET", "/dashboard", nil, rotated).Code != 503 {
		t.Fatal("session backend failure must fail closed")
	}
}
