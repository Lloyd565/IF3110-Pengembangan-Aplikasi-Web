package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/httpx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/workflow"
)

type apiClient struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
}

func (c *apiClient) do(method, path string, body any, headers ...string) (int, json.RawMessage) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	r, _ := http.NewRequest(method, c.base+path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", c.base)
	r.Header.Set("X-CSRF-Token", c.csrf)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.client.Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var payload struct {
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&payload)
	if payload.Data == nil {
		return res.StatusCode, payload.Error
	}
	return res.StatusCode, payload.Data
}

func login(t *testing.T, srv *httptest.Server, username string) *apiClient {
	t.Helper()
	client := *srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	c := &apiClient{t: t, base: srv.URL, client: &client}
	_, raw := c.do("GET", "/api/auth/csrf", nil)
	var tok struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(raw, &tok)
	c.csrf = tok.CSRF
	code, raw := c.do("POST", "/api/auth/login", map[string]string{"username": username, "password": "workflow-secret-password"})
	if code != 200 {
		t.Fatalf("login %s: %d %s", username, code, raw)
	}
	json.Unmarshal(raw, &tok)
	c.csrf = tok.CSRF
	return c
}

func TestIntegrationWorkflowAPI(t *testing.T) {
	db := testdb.Fresh(t, "workflow_api")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("workflow-secret-password")
	if _, err := db.Exec(ctx, `INSERT INTO users (id, username, password_hash) VALUES (1,'alice',$1),(2,'bob',$1)`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO groups (id, name) VALUES (10,'app'),(20,'ro');
		INSERT INTO group_members VALUES (10,1),(20,2);
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc)
			VALUES ('aaaaaaaa-0000-0000-0000-000000000001','A','h',5432,'d','u','\x00');
		INSERT INTO db_grants VALUES (10,'aaaaaaaa-0000-0000-0000-000000000001','edit'),(20,'aaaaaaaa-0000-0000-0000-000000000001','view');
		INSERT INTO group_leaders (group_id, user_id) VALUES (10,1),(20,2);`); err != nil {
		t.Fatal(err)
	}
	s := &httpx.Server{DB: db, Authz: &authz.Authorizer{DB: db}, SPAEnabled: true}
	s.Workflow = &workflow.Service{DB: db, Authz: s.Authz}
	if err := s.ConfigureAuth(auth.DefaultPolicy(), []byte(strings.Repeat("k", 32)), true); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(s.Routes())
	t.Cleanup(srv.Close)
	base := "/api/db/aaaaaaaa-0000-0000-0000-000000000001"

	alice, bob := login(t, srv, "alice"), login(t, srv, "bob")

	if code, _ := bob.do("POST", base+"/migrations", map[string]any{"name": "x", "sql_up": "SELECT 1", "sql_down": "SELECT 1"}); code != 403 {
		t.Fatalf("View Only membuat migration: %d", code)
	}
	if code, _ := alice.do("POST", base+"/migrations", map[string]any{"name": "x", "sql_up": "SELECT 1"}); code != 422 {
		t.Fatalf("tanpa down/irreversible: %d", code)
	}
	code, raw := alice.do("POST", base+"/migrations", map[string]any{"name": "tambah kolom", "sql_up": "SELECT 1", "sql_down": "SELECT 2"})
	var m struct{ ID, RevisionID string }
	json.Unmarshal(raw, &struct {
		ID         *string `json:"id"`
		RevisionID *string `json:"revision_id"`
	}{&m.ID, &m.RevisionID})
	if code != 201 || m.RevisionID == "" {
		t.Fatalf("draft: %d %s", code, raw)
	}

	code, raw = alice.do("POST", base+"/requests", map[string]any{"migration_id": m.ID, "revision_id": m.RevisionID, "action": "apply", "mode": "normal"})
	var req struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(raw, &req)
	if code != 201 || req.Status != "draft" {
		t.Fatalf("propose: %d %s", code, raw)
	}
	if code, raw = alice.do("POST", base+"/requests/"+req.ID+"/submit", map[string]any{}); code != 200 {
		t.Fatalf("submit: %d %s", code, raw)
	}
	alice.do("POST", base+"/requests/"+req.ID+"/review", map[string]string{"decision": "approve"})
	code, raw = bob.do("POST", base+"/requests/"+req.ID+"/review", map[string]string{"decision": "approve"})
	json.Unmarshal(raw, &req)
	if code != 200 || req.Status != "approved" {
		t.Fatalf("approval leader View Only: %d %s", code, raw)
	}
	if code, _ = bob.do("POST", base+"/requests/"+req.ID+"/enqueue", map[string]any{}, "Idempotency-Key", "key-123456"); code != 403 {
		t.Fatalf("View Only enqueue: %d", code)
	}
	if code, raw = alice.do("POST", base+"/requests/"+req.ID+"/enqueue", map[string]any{}, "Idempotency-Key", "key-123456"); code != 503 {
		t.Fatalf("enqueue tanpa queue: %d %s", code, raw)
	}
	if code, raw = bob.do("GET", base+"/history?hide_rollback=true", nil); code != 200 || string(raw) != "[]" {
		t.Fatalf("history: %d %s", code, raw)
	}
}
