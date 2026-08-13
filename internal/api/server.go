package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/eordano/cccp/internal/store"
)

const (
	HeaderHostID   = "X-Host-Id"
	HeaderHostName = "X-Host-Name"

	MaxBlobBytes  = 1 << 30
	WarnBlobBytes = 32 << 20

	scanBufInit = 64 * 1024
	maxScanLine = 4 * 1024 * 1024
)

type Server struct {
	Store  *store.Store
	Token  string
	Log    *slog.Logger
	Events *Broadcaster
	Watch  http.Handler
}

func (s *Server) Routes() http.Handler {
	if s.Events == nil {
		s.Events = NewBroadcaster()
	}
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /ingest", s.handleIngest)
	apiMux.HandleFunc("PUT /blob/{sha}", s.handlePutBlob)
	apiMux.HandleFunc("GET /current", s.handleCurrent)
	apiMux.HandleFunc("POST /log/append", s.handleLogAppend)
	apiMux.HandleFunc("GET /log/since", s.handleLogSince)
	apiMux.HandleFunc("GET /events", s.handleEvents)

	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})

	authed := s.auth(apiMux)
	outer.Handle("POST /ingest", authed)
	outer.Handle("PUT /blob/{sha}", authed)
	outer.Handle("GET /current", authed)
	outer.Handle("POST /log/append", authed)
	outer.Handle("GET /log/since", authed)
	outer.Handle("GET /events", authed)
	if s.Watch != nil {

		outer.HandleFunc("GET /login", s.handleLoginPage)
		outer.HandleFunc("POST /login", s.handleLogin)
		outer.Handle("/", s.cookieAuth(s.Watch))
	} else {
		outer.Handle("/", authed)
	}
	return outer
}

const cookieToken = "cccp_token"

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {

			if c, err := r.Cookie(cookieToken); err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cookieAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); bearer != "" {
			if subtle.ConstantTimeCompare([]byte(bearer), []byte(s.Token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if c, err := r.Cookie(cookieToken); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(loginHTML))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	token := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieToken,
		Value:    token,
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 365,
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

const loginHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>cccp - login</title>
<style>
  body { background: #0a0a0b; color: #e0e0e0; font-family: system-ui; display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; }
  form { background: #141416; border: 1px solid #2a2a2e; border-radius: 8px; padding: 2rem; max-width: 320px; width: 100%; }
  h1 { font-size: 1.2rem; margin: 0 0 1rem; }
  input[type=password] { width: 100%; padding: 0.5rem; background: #1a1a1e; border: 1px solid #3a3a3e; border-radius: 4px; color: #e0e0e0; font-size: 1rem; box-sizing: border-box; }
  button { margin-top: 1rem; padding: 0.5rem 1rem; background: #2563eb; border: none; border-radius: 4px; color: white; cursor: pointer; font-size: 1rem; width: 100%; }
  button:hover { background: #1d4ed8; }
  .error { color: #ef4444; font-size: 0.85rem; margin-top: 0.5rem; }
</style>
</head>
<body>
<form method="POST" action="/login">
  <h1>pravda</h1>
  <input type="password" name="token" placeholder="token" autofocus>
  <button type="submit">enter</button>
</form>
</body>
</html>`

func isTLS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func httpError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}
