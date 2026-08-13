package pravda

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed public/*
var publicFS embed.FS

type WatchServer struct {
	Store    SessionStore
	assets   fs.FS
	upgrader websocket.Upgrader

	clientsMu sync.Mutex
	clients   map[*websocket.Conn]struct{}

	live   *liveHub
	cache  *liveCache
	parsed *parsedCache

	sidebar sidebarState

	titles sync.Map

	versions sync.Map
}

type sidebarState struct {
	mu    sync.RWMutex
	valid bool
	flat  []*flatItem
}

func NewWatchServer(store SessionStore) *WatchServer {
	assets, _ := fs.Sub(publicFS, "public")
	return &WatchServer{
		Store:    store,
		assets:   assets,
		upgrader: websocket.Upgrader{CheckOrigin: checkOrigin},
		clients:  map[*websocket.Conn]struct{}{},
		live:     newLiveHub(),
		cache:    newLiveCache(),
		parsed:   newParsedCache(),
	}
}

func (s *WatchServer) Run(ctx context.Context) {
	ch := s.Store.Subscribe(ctx)
	for path := range ch {
		s.onLogEvent(path)
	}
}

func (s *WatchServer) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/ws/live/{path...}", s.handleLiveWS)
	mux.HandleFunc("GET /test", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok - pravda")
	})

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /session/{path...}", s.handleSession)
	mux.HandleFunc("GET /subagent/{path...}", s.handleSubAgent)
	mux.HandleFunc("POST /settings/{key}", s.handleSettings)

	mux.HandleFunc("/", s.handleStatic)
	return corsMiddleware(mux)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && originAllowed(origin, r.Host)
		if origin != "" {
			w.Header().Set("Vary", "Origin")
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions {
			if allowed {
				if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
					w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' ws: wss:; object-src 'none'; base-uri 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *WatchServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" {
		rel = "index.html"
	}
	f, err := s.assets.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f.Close()
	if mt := mime.TypeByExtension(filepath.Ext(rel)); mt != "" {
		w.Header().Set("Content-Type", mt)
	}
	http.ServeFileFS(w, r, s.assets, rel)
}

func (s *WatchServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer conn.Close()

	s.clientsMu.Lock()
	s.clients[conn] = struct{}{}
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, conn)
		s.clientsMu.Unlock()
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (s *WatchServer) broadcast(msg string) {
	s.clientsMu.Lock()
	snap := make([]*websocket.Conn, 0, len(s.clients))
	for c := range s.clients {
		snap = append(snap, c)
	}
	s.clientsMu.Unlock()

	payload := []byte(msg)
	var dead []*websocket.Conn
	for _, c := range snap {
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			dead = append(dead, c)
		}
	}
	if len(dead) > 0 {
		s.clientsMu.Lock()
		for _, c := range dead {
			c.Close()
			delete(s.clients, c)
		}
		s.clientsMu.Unlock()
	}
}

func (s *WatchServer) onLogEvent(rel string) {
	s.sidebar.invalidate()
	s.versions.Store(rel, time.Now().UnixNano())
	s.broadcastLive(rel)
	if parent := parentSessionOf(rel); parent != "" {
		s.broadcastLive(parent)
	}
	if !isAgentPath(rel) {
		s.broadcast("new-session:" + rel)
	}
}

func (s *WatchServer) pathVersion(rel string) int64 {
	if v, ok := s.versions.Load(rel); ok {
		if n, ok := v.(int64); ok {
			return n
		}
	}
	return 0
}

func (ss *sidebarState) invalidate() {
	ss.mu.Lock()
	ss.valid = false
	ss.mu.Unlock()
}

func isInside(path, base string) bool {
	ap, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	ab, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(ab, ap)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

func originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if u.Host == host {
		return true
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := r.Host

	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	return origin == host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
