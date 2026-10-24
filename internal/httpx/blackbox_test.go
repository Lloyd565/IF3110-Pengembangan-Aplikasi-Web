package httpx_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/httpx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type blackboxApp struct {
	db     *pgxpool.Pool
	server *httpx.Server
	url    string
	client *http.Client
}

func newBlackboxApp(t *testing.T, name string, production bool) *blackboxApp {
	t.Helper()
	db := testdb.Fresh(t, "blackbox_"+name)
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword("blackbox-secret-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (id, username, password_hash, is_active)
		VALUES (1, 'Budi', $1, true), (2, 'Inactive', $1, false)`, hash); err != nil {
		t.Fatal(err)
	}
	s := &httpx.Server{DB: db, Authz: &authz.Authorizer{DB: db}}
	if err := s.ConfigureAuth(auth.DefaultPolicy(), []byte(strings.Repeat("k", 32)), production); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewTLSServer(s.Routes())
	t.Cleanup(httpServer.Close)
	client := httpServer.Client()
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Timeout = 20 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &blackboxApp{db: db, server: s, url: httpServer.URL, client: client}
}

func (a *blackboxApp) request(t *testing.T, method, path string, values url.Values) (*http.Response, string) {
	t.Helper()
	var reader io.Reader
	if values != nil {
		reader = strings.NewReader(values.Encode())
	}
	r, err := http.NewRequest(method, a.url+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if values != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", a.url)
	}
	return a.send(t, a.client, r)
}

func (a *blackboxApp) send(t *testing.T, client *http.Client, r *http.Request) (*http.Response, string) {
	t.Helper()
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

func (a *blackboxApp) requestWithCookie(t *testing.T, path string, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest("GET", a.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(cookie)
	client := *a.client
	client.Jar = nil
	return a.send(t, &client, r)
}

func responseSessionCookie(t *testing.T, r *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	t.Fatal("expected a new session cookie")
	return nil
}

func csrfFromBody(t *testing.T, body string) string {
	t.Helper()
	matches := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(matches) != 2 {
		t.Fatal("response has no CSRF form input")
	}
	return matches[1]
}

func (a *blackboxApp) login(t *testing.T) *http.Cookie {
	t.Helper()
	_, body := a.request(t, "GET", "/login", nil)
	r, body := a.request(t, "POST", "/login", url.Values{"username": {"bUdI"}, "password": {"blackbox-secret-password"}, "csrf_token": {csrfFromBody(t, body)}})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard" {
		t.Fatalf("login status = %d", r.StatusCode)
	}
	for _, c := range r.Cookies() {
		if c.Value != "" && strings.HasSuffix(c.Name, "session") {
			if strings.Contains(body, c.Value) {
				t.Fatal("session identifier leaked in response body")
			}
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestIntegrationBlackboxHTTPSDevelopmentCookies(t *testing.T) {
	a := newBlackboxApp(t, "https_dev", false)
	r, _ := a.request(t, "GET", "/login", nil)
	for _, cookie := range r.Cookies() {
		if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatal("HTTPS pre-session cookie flags are incomplete")
		}
	}
	cookie := a.login(t)
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("HTTPS session cookie flags are incomplete")
	}
	r, _ = a.request(t, "GET", "/dashboard", nil)
	if r.StatusCode != 200 {
		t.Fatalf("cookie jar did not retain login: %d", r.StatusCode)
	}
}

func TestIntegrationBlackboxConcurrentRotation(t *testing.T) {
	a := newBlackboxApp(t, "parallel", true)
	cookie := a.login(t)
	if _, err := a.db.Exec(context.Background(), `UPDATE sessions SET rotated_at = now() - interval '16 minutes' WHERE user_id = 1`); err != nil {
		t.Fatal(err)
	}
	// All requests deliberately use the same old cookie, as parallel browser requests do.
	transportClient := *a.client
	transportClient.Jar = nil
	var wg sync.WaitGroup
	results := make(chan string, 96)
	for range 96 {
		wg.Go(func() {
			r, err := http.NewRequest("GET", a.url+"/dashboard", nil)
			if err != nil {
				results <- "request construction failed"
				return
			}
			r.AddCookie(cookie)
			response, err := transportClient.Do(r)
			if err != nil {
				results <- "request failed"
				return
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				results <- http.StatusText(response.StatusCode)
				return
			}
			for _, c := range response.Cookies() {
				if c.Name == cookie.Name && c.Value != "" {
					results <- c.Value
					return
				}
			}
			results <- "rotation cookie missing"
		})
	}
	wg.Wait()
	close(results)
	var winner string
	for result := range results {
		if !auth.ValidToken(result) {
			t.Errorf("parallel request failed: %s", result)
			continue
		}
		if winner == "" {
			winner = result
		}
		if result != winner {
			t.Error("parallel responses did not converge on one cookie")
		}
	}
}

func TestIntegrationBlackboxLoginAndCSRF(t *testing.T) {
	a := newBlackboxApp(t, "login_csrf", true)
	for _, path := range []string{"/dashboard", "/admin/users", "/admin/groups", "/admin/connections", "/admin/grants", "/db/anything/schema"} {
		t.Run("anonymous "+path, func(t *testing.T) {
			r, _ := a.request(t, "GET", path, nil)
			if r.StatusCode != 303 || r.Header.Get("Location") != "/login" {
				t.Fatalf("protected endpoint returned %d", r.StatusCode)
			}
		})
	}
	_, body := a.request(t, "GET", "/login", nil)
	pre := csrfFromBody(t, body)
	_, secondTab := a.request(t, "GET", "/login", nil)
	if csrfFromBody(t, secondTab) != pre {
		t.Fatal("opening a second tab invalidated the first login form")
	}
	for _, tc := range []struct{ name, username, password string }{
		{"wrong password", "Budi", "wrong-secret"},
		{"unknown user", "Nobody", "blackbox-secret-password"},
		{"inactive user", "Inactive", "blackbox-secret-password"},
		{"SQL injection", "' OR 1=1 --", "blackbox-secret-password"},
		{"HTML injection", "<img src=x onerror=alert(1)>", "wrong-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, body := a.request(t, "POST", "/login", url.Values{"username": {tc.username}, "password": {tc.password}, "csrf_token": {pre}})
			if r.StatusCode != 401 || !strings.Contains(body, "Username atau password salah.") || strings.Contains(body, tc.password) {
				t.Fatal("invalid credentials were accepted or disclosed")
			}
			if tc.name == "HTML injection" && strings.Contains(body, tc.username) {
				t.Fatal("username was rendered as HTML")
			}
			for _, c := range r.Cookies() {
				if strings.HasSuffix(c.Name, "session") && c.Value != "" {
					t.Fatal("failed login issued a session")
				}
			}
		})
	}
	for _, name := range []string{"missing csrf", "wrong csrf", "query csrf", "cross origin", "null origin", "cross site", "oversize form", "forged pre-session"} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{"username": {"Budi"}, "password": {"blackbox-secret-password"}, "csrf_token": {pre}}
			path, expected := "/login", 403
			switch name {
			case "missing csrf":
				form.Del("csrf_token")
			case "wrong csrf":
				form.Set("csrf_token", "wrong")
			case "query csrf":
				form.Del("csrf_token")
				path += "?csrf_token=" + url.QueryEscape(pre)
			case "oversize form":
				form.Set("payload", strings.Repeat("x", 201<<10))
				expected = 413
			case "forged pre-session":
				form.Set("csrf_token", pre+"forged")
			}
			r, err := http.NewRequest("POST", a.url+path, strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			switch name {
			case "cross origin":
				r.Header.Set("Origin", "https://evil.test")
			case "null origin":
				r.Header.Set("Origin", "null")
			case "cross site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			client := a.client
			if name == "forged pre-session" {
				copyClient := *client
				copyClient.Jar = nil
				client = &copyClient
				r.AddCookie(&http.Cookie{Name: "__Host-login_csrf", Value: pre + "forged"})
			}
			response, _ := a.send(t, client, r)
			if response.StatusCode != expected {
				t.Fatalf("CSRF rejection = %d, want %d", response.StatusCode, expected)
			}
		})
	}
	t.Run("password from URL is ignored", func(t *testing.T) {
		r, body := a.request(t, "POST", "/login?username=Budi&password=blackbox-secret-password", url.Values{"csrf_token": {pre}})
		if r.StatusCode != 401 || strings.Contains(body, "blackbox-secret-password") {
			t.Fatal("URL credentials were accepted or reflected")
		}
	})
	cookie := a.login(t)
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Domain != "" || cookie.Path != "/" {
		t.Fatal("production cookie flags")
	}
	r, body := a.request(t, "GET", "/dashboard", nil)
	if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("dashboard/headers")
	}
	csrf := csrfFromBody(t, body)
	if csrf == pre {
		t.Fatal("login reused the pre-session CSRF token")
	}
	for _, path := range []string{"/admin/users", "/admin/groups", "/admin/connections", "/admin/grants"} {
		t.Run("non-admin "+path, func(t *testing.T) {
			r, _ := a.request(t, "GET", path, nil)
			if r.StatusCode != 403 {
				t.Fatalf("admin endpoint returned %d", r.StatusCode)
			}
			r, _ = a.request(t, "POST", path, url.Values{"csrf_token": {csrf}})
			if r.StatusCode != 403 {
				t.Fatalf("non-admin POST returned %d", r.StatusCode)
			}
		})
	}
	t.Run("logout requires CSRF", func(t *testing.T) {
		r, _ := a.request(t, "POST", "/logout", nil)
		if r.StatusCode != 403 {
			t.Fatal("logout accepted missing CSRF")
		}
		r, _ = a.request(t, "GET", "/dashboard", nil)
		if r.StatusCode != 200 {
			t.Fatal("rejected logout changed authentication state")
		}
	})
	t.Run("logout and replay", func(t *testing.T) {
		r, _ := a.request(t, "POST", "/logout", url.Values{"csrf_token": {csrf}})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/login" {
			t.Fatal("logout failed")
		}
		r, _ = a.requestWithCookie(t, "/dashboard", cookie)
		if r.StatusCode != 303 {
			t.Fatal("backend accepted a replay after logout")
		}
		r, _ = a.request(t, "GET", "/dashboard", nil)
		if r.StatusCode != 303 {
			t.Fatal("cookie jar remained authenticated after logout")
		}
	})
	t.Run("session backend outage", func(t *testing.T) {
		a.db.Close()
		r, _ := a.requestWithCookie(t, "/dashboard", cookie)
		if r.StatusCode != 503 {
			t.Fatalf("backend outage returned %d", r.StatusCode)
		}
	})
}

func TestIntegrationBlackboxRotationAndExpiry(t *testing.T) {
	a := newBlackboxApp(t, "expiration", true)
	cookie := a.login(t)
	ctx := context.Background()
	t.Run("periodic rotation", func(t *testing.T) {
		if _, err := a.db.Exec(ctx, `UPDATE sessions SET rotated_at = now() - interval '16 minutes' WHERE user_id = 1`); err != nil {
			t.Fatal(err)
		}
		r, _ := a.requestWithCookie(t, "/dashboard", cookie)
		if r.StatusCode != 200 {
			t.Fatal("rotation denied a valid session")
		}
		rotated := responseSessionCookie(t, r, cookie.Name)
		if rotated.Value == cookie.Value || !rotated.Expires.Equal(cookie.Expires) {
			t.Fatal("rotation failed or extended absolute expiry")
		}
		r, _ = a.requestWithCookie(t, "/dashboard", cookie)
		if r.StatusCode != 200 || responseSessionCookie(t, r, cookie.Name).Value != rotated.Value {
			t.Fatal("grace did not recover the winning cookie")
		}
		if _, err := a.db.Exec(ctx, `UPDATE sessions SET prev_valid_until = now() - interval '1 second' WHERE user_id = 1`); err != nil {
			t.Fatal(err)
		}
		r, _ = a.requestWithCookie(t, "/dashboard", cookie)
		if r.StatusCode != 303 {
			t.Fatal("previous token accepted after grace")
		}
		cookie = rotated
	})
	t.Run("privilege rotation", func(t *testing.T) {
		if _, err := a.db.Exec(ctx, `UPDATE users SET is_platform_admin = true WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		if err := a.server.Sessions.MarkForceRotate(ctx, 1); err != nil {
			t.Fatal(err)
		}
		r, _ := a.requestWithCookie(t, "/admin/users", cookie)
		if r.StatusCode != 501 {
			t.Fatal("middleware did not refresh the admin principal")
		}
		rotated := responseSessionCookie(t, r, cookie.Name)
		if rotated.Value == cookie.Value || !rotated.Expires.Equal(cookie.Expires) {
			t.Fatal("privilege change failed to rotate")
		}
		cookie = rotated
	})
	for _, tc := range []struct{ name, sql string }{
		{"idle timeout", `UPDATE sessions SET last_seen_at = now() - interval '31 minutes' WHERE user_id = 1 AND revoked_at IS NULL`},
		{"absolute expiration", `UPDATE sessions SET absolute_expires_at = now() - interval '1 second', last_seen_at = now() WHERE user_id = 1 AND revoked_at IS NULL`},
		{"inactive user", `UPDATE users SET is_active = false WHERE id = 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.db.Exec(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			r, _ := a.requestWithCookie(t, "/dashboard", cookie)
			if r.StatusCode != 303 {
				t.Fatalf("invalid session status = %d", r.StatusCode)
			}
			if _, err := a.db.Exec(ctx, `UPDATE users SET is_active = true WHERE id = 1`); err != nil {
				t.Fatal(err)
			}
			r, _ = a.requestWithCookie(t, "/dashboard", cookie)
			if r.StatusCode != 303 {
				t.Fatal("reactivation resurrected the old session")
			}
			// Use a separate browser context for each new authentication.
			a.client.Jar, _ = cookiejar.New(nil)
			cookie = a.login(t)
		})
	}
	t.Run("access revocation", func(t *testing.T) {
		if err := a.server.Sessions.RevokeAllForUser(ctx, 1); err != nil {
			t.Fatal(err)
		}
		r, _ := a.requestWithCookie(t, "/dashboard", cookie)
		if r.StatusCode != 303 {
			t.Fatal("access revocation did not invalidate session")
		}
	})
}

func TestIntegrationBlackboxLoginRateLimit(t *testing.T) {
	a := newBlackboxApp(t, "rate_limit", true)
	_, body := a.request(t, "GET", "/login", nil)
	form := url.Values{"username": {"Budi"}, "password": {"wrong"}, "csrf_token": {csrfFromBody(t, body)}}
	for range 10 {
		r, _ := a.request(t, "POST", "/login", form)
		if r.StatusCode != 401 {
			t.Fatalf("early login limit: %d", r.StatusCode)
		}
	}
	r, _ := a.request(t, "POST", "/login", form)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatal("login brute-force limit failed")
	}
}
