package httpx

import (
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/web"
	"github.com/google/uuid"
)

func (s *Server) requestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(telemetry.With(r.Context(), telemetry.Info{CorrelationID: id, Database: "platform"})))
	})
}

func (s *Server) apiLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		status := &responseStatus{ResponseWriter: w}
		completed := false
		defer func() {
			code := status.status
			if code == 0 {
				code = 200
				if !completed {
					code = 500
				}
			}
			slog.InfoContext(r.Context(), "http.request", "method", r.Method, "status", code, "duration_ms", time.Since(start).Milliseconds(), "request_id", w.Header().Get("X-Request-ID"))
		}()
		next.ServeHTTP(status, r)
		completed = true
	})
}

func (s *Server) spaPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api" {
		apiError(w, 404, "not_found", "Endpoint tidak ditemukan.")
		return
	}
	data, err := fs.ReadFile(web.FS, "static/spa/index.html")
	if err != nil {
		apiError(w, 503, "frontend_unbuilt", "Frontend belum dibangun. Jalankan npm ci dan npm run build dari folder frontend.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) spaAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	name = "static/" + name
	info, err := fs.Stat(web.FS, name)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(web.FS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	typeName := mime.TypeByExtension(path.Ext(name))
	if typeName == "" {
		typeName = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", typeName)
	if strings.Contains(name, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Write(data)
}
