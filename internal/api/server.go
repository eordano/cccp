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
	Store *store.Store
	Token string
	// PushToken, when non-empty, authorizes ONLY the push endpoints
	// (POST /ingest, PUT /blob/{sha}, POST /log/append) via bearer auth.
	// It never grants reads: /current, /log/since, /events and the pravda
	// UI stay full-token-only, so a host holding it can ship its own state
	// but cannot read anything back.
	PushToken string
	// ReadToken, when non-empty, authorizes ONLY GET/HEAD on the readRoutes
	// allowlist (readtoken.go). It is refused with 403 on every write, every
	// review decision and the login form, so a holder can read sessions and
	// SAE views but can never approve its own review-gate proposals.
	ReadToken string
	Log       *slog.Logger
	Events    *Broadcaster
	Watch     http.Handler
	SaeDir    string
	// SaeModelsDir receives the weights an import carries (sae.safetensors),
	// where on-demand scoring looks for them.
	SaeModelsDir string
	Precheck     string
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
	apiMux.HandleFunc("POST /sae/import", s.handleSaeImport)
	apiMux.HandleFunc("GET /sae/feedback", s.handleSaeFeedbackExport)
	apiMux.HandleFunc("GET /review/api/list", s.handleReviewList)
	apiMux.HandleFunc("GET /review/api/show", s.handleReviewShow)
	apiMux.HandleFunc("POST /review/api/decide", s.handleReviewDecide)
	apiMux.HandleFunc("GET /review", s.handleReviewPage)
	apiMux.HandleFunc("GET /review/show", s.handleReviewShowPage)
	apiMux.HandleFunc("POST /review/decide", s.handleReviewDecideForm)

	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})

	authed := s.auth(apiMux)
	pushAuthed := s.pushAuth(apiMux)
	outer.Handle("POST /ingest", pushAuthed)
	outer.Handle("PUT /blob/{sha}", pushAuthed)
	outer.Handle("GET /current", authed)
	outer.Handle("POST /log/append", pushAuthed)
	outer.Handle("GET /log/since", authed)
	outer.Handle("GET /events", authed)
	outer.Handle("POST /sae/import", pushAuthed)
	outer.Handle("GET /sae/feedback", authed)
	outer.Handle("/review/api/", authed)
	if s.Watch != nil {
		outer.Handle("/review", s.cookieAuth(apiMux))
		outer.Handle("/review/", s.cookieAuth(apiMux))

		outer.HandleFunc("GET /login", s.handleLoginPage)
		outer.HandleFunc("POST /login", s.handleLogin)
		outer.Handle("/", s.cookieAuth(s.Watch))
	} else {
		outer.Handle("/", authed)
	}
	return s.readGate(outer)
}

const cookieToken = "cccp_token"

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if readOnly(r.Context()) {
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

func (s *Server) pushAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if s.PushToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.PushToken)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(cookieToken); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (s *Server) cookieAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || readOnly(r.Context()) {
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
  .username { position: absolute; left: -9999px; width: 1px; height: 1px; opacity: 0; }
  button { margin-top: 1rem; padding: 0.5rem 1rem; background: #2563eb; border: none; border-radius: 4px; color: white; cursor: pointer; font-size: 1rem; width: 100%; }
  button:hover { background: #1d4ed8; }
  .error { color: #ef4444; font-size: 0.85rem; margin-top: 0.5rem; }
</style>
</head>
<body>
<form method="POST" action="/login" autocomplete="on">
  <h1>pravda</h1>
  <input type="text" name="username" value="pravda" class="username" autocomplete="username" readonly tabindex="-1" aria-hidden="true">
  <input type="password" name="token" id="token" placeholder="token" autocomplete="current-password" autofocus>
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
