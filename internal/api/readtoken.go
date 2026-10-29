package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

var readRoutes = []string{
	"GET /healthz",
	"GET /current",
	"GET /log/since",
	"GET /events",
	"GET /sae/feedback",
	"GET /review/api/list",
	"GET /review/api/show",
	"GET /review",
	"GET /review/show",

	"GET /{$}",
	"GET /session/{path...}",
	"GET /subagent/{path...}",
	"GET /sessions",
	"GET /sessions/{id...}",
	"GET /index-panel/{path...}",
	"GET /files",
	"GET /files/{path...}",
	"GET /sae",
	"GET /sae/{$}",
	"GET /sae/features",
	"GET /sae/feature/{id}",
	"GET /sae/settings",
	"GET /sae/session/{path...}",
	"GET /sae/daily/{file...}",
	"GET /sae/similar/{id}",
	"GET /ws",
	"GET /ws/live/{path...}",

	"GET /styles.css",
	"GET /loading.css",
	"GET /sae.css",
	"GET /pravda.js",
	"GET /loading.js",
	"GET /sae.js",
	"GET /htmx-ws.js",
	"GET /htmx.min.js",
}

type readRoleKey struct{}

func newReadRouteMux() *http.ServeMux {
	m := http.NewServeMux()
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, p := range readRoutes {
		m.Handle(p, ok)
	}
	return m
}

func readOnly(ctx context.Context) bool {
	v, _ := ctx.Value(readRoleKey{}).(bool)
	return v
}

func (s *Server) bearerIsReadToken(r *http.Request) bool {
	if s.ReadToken == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.ReadToken)) == 1
}

func (s *Server) readGate(next http.Handler) http.Handler {
	reads := newReadRouteMux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.bearerIsReadToken(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "forbidden: read-only token", http.StatusForbidden)
			return
		}
		if _, pattern := reads.Handler(r); pattern == "" {
			http.Error(w, "forbidden: read-only token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), readRoleKey{}, true)))
	})
}
