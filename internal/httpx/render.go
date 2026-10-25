package httpx

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"net/url"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/web"
)

type view struct {
	User      reqctx.Principal
	LoggedIn  bool
	CSRFToken string
	Data      any
}

var templateFuncs = template.FuncMap{"pathEscape": url.PathEscape}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	t, err := template.New("").Funcs(templateFuncs).ParseFS(web.FS, "templates/layout.html", "templates/"+page)
	if err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	p, ok := reqctx.CurrentPrincipal(r.Context())
	v := view{User: p, LoggedIn: ok, CSRFToken: reqctx.CSRFToken(r.Context()), Data: data}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", v); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "error.html", struct {
		Status  int
		Text    string
		Message string
	}{status, http.StatusText(status), message})
}
