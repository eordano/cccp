package pravda

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"github.com/eordano/cccp/internal/sae"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

//go:embed public/*
var publicFS embed.FS

type WatchServer struct {
	Store     SessionStore
	Sae       *sae.Index
	SaeScorer saeExecutor
	SaeDir    string
	assets    fs.FS

	clientsMu sync.Mutex
	clients   map[*websocket.Conn]struct{}

	live   *liveHub
	cache  *liveCache
	parsed *parsedCache

	sidebar sidebarState

	titles    sync.Map
	factsOnce sync.Once

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
		Store:   withConverters(store),
		assets:  assets,
		clients: map[*websocket.Conn]struct{}{},
		live:    newLiveHub(),
		cache:   newLiveCache(),
		parsed:  newParsedCache(),
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
	mux.HandleFunc("GET /sae", s.handleSaeIndex)
	mux.HandleFunc("GET /sae/{$}", s.handleSaeIndex)
	mux.HandleFunc("GET /sae/features", s.handleSaeFeatures)
	mux.HandleFunc("GET /sae/feature/{id}", s.handleSaeFeature)
	mux.HandleFunc("GET /sae/settings", s.handleSaeSettings)
	mux.HandleFunc("GET /sae/session/{path...}", s.handleSaeSession)
	mux.HandleFunc("GET /sae/daily/{file...}", s.handleSaeDaily)
	mux.HandleFunc("GET /sae/similar/{id}", s.handleSaeSimilar)
	mux.HandleFunc("POST /feedback/{path...}", s.handleFeedback)
	mux.HandleFunc("GET /subagent/{path...}", s.handleSubAgent)
	mux.HandleFunc("POST /settings/{key}", s.handleSettings)
	s.registerIndexRoutes(mux)

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
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1)

	s.clientsMu.Lock()
	s.clients[conn] = struct{}{}
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, conn)
		s.clientsMu.Unlock()
	}()

	for {
		if _, _, err := conn.Read(r.Context()); err != nil {
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
		if err := c.Write(context.Background(), websocket.MessageText, payload); err != nil {
			dead = append(dead, c)
		}
	}
	if len(dead) > 0 {
		s.clientsMu.Lock()
		for _, c := range dead {
			c.CloseNow()
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

func (s *WatchServer) events(ctx context.Context, rel string, ver int64) ([]*RawEvent, error) {
	if events, ok := s.parsed.getCached(rel, ver); ok {
		return events, nil
	}
	if ls, ok := s.Store.(sessionLineStore); ok {
		after := s.parsed.cursor(rel)
		lines, cursor, err := ls.SessionLines(ctx, rel, after)
		if err == nil {
			if after == 0 && len(lines) == 0 {
				return nil, &pathNotFoundError{rel}
			}
			return s.parsed.appendLines(rel, ver, after, cursor, lines), nil
		}
		if !errors.Is(err, errNoLineStore) {
			return nil, err
		}
	}
	rc, err := s.Store.OpenSession(ctx, rel)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return s.parsed.parse(rc, rel, ver)
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
