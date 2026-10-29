package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newThreeTokenServer(t *testing.T) http.Handler {
	t.Helper()
	s := &Server{
		Store:     newTestStore(t),
		Token:     "full-token",
		PushToken: "push-token",
		ReadToken: "read-token",
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Watch:     &stubHandler{},
	}
	return s.Routes()
}

type route struct{ method, path string }

func doBounded(h http.Handler, method, path, bearer string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(method, path, strings.NewReader("")).WithContext(ctx)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

var (
	apiReadRoutes = []route{
		{http.MethodGet, "/current"},
		{http.MethodGet, "/log/since?since=0"},
		{http.MethodGet, "/events"},
		{http.MethodGet, "/sae/feedback"},
		{http.MethodGet, "/review/api/list"},
		{http.MethodGet, "/review"},
		{http.MethodHead, "/log/since?since=0"},
	}
	pravdaReadRoutes = []route{
		{http.MethodGet, "/"},
		{http.MethodGet, "/sessions"},
		{http.MethodGet, "/sessions/abc?records=1"},
		{http.MethodGet, "/session/x"},
		{http.MethodGet, "/subagent/x"},
		{http.MethodGet, "/index-panel/x"},
		{http.MethodGet, "/files"},
		{http.MethodGet, "/files/skills/x/SKILL.md"},
		{http.MethodGet, "/sae/features"},
		{http.MethodGet, "/sae/feature/1"},
		{http.MethodGet, "/sae/session/x"},
		{http.MethodGet, "/sae/daily/2026-10-11.html"},
		{http.MethodGet, "/sae/similar/1"},
		{http.MethodGet, "/styles.css"},
		{http.MethodHead, "/sessions"},
	}
	writeRoutes = []route{
		{http.MethodPost, "/ingest"},
		{http.MethodPut, "/blob/deadbeef"},
		{http.MethodPost, "/log/append"},
		{http.MethodPost, "/sae/import"},
	}
	pravdaWriteRoutes = []route{
		{http.MethodPost, "/feedback/x"},
		{http.MethodPost, "/settings/theme"},
	}
	reviewActions = []route{
		{http.MethodPost, "/review/api/decide"},
		{http.MethodPost, "/review/decide"},
	}
	unlistedRoutes = []route{
		{http.MethodGet, "/test"},
		{http.MethodGet, "/not-a-route-yet"},
		{http.MethodGet, "/review/api/new-thing"},
		{http.MethodPost, "/sessions"},
		{http.MethodDelete, "/sessions/x"},
		{http.MethodGet, "/x/../ingest"},
	}
)

func TestReadToken_ReadsAllowed(t *testing.T) {
	h := newThreeTokenServer(t)
	for _, c := range append(append([]route{}, apiReadRoutes...), pravdaReadRoutes...) {
		code := doBounded(h, c.method, c.path, "read-token")
		if code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusSeeOther {
			t.Errorf("%s %s with read token: got %d, want authorized", c.method, c.path, code)
		}
	}
}

func TestReadToken_WritesForbidden(t *testing.T) {
	h := newThreeTokenServer(t)
	all := append(append(append([]route{}, writeRoutes...), pravdaWriteRoutes...), reviewActions...)
	all = append(all, route{http.MethodPost, "/login"})
	for _, c := range all {
		if code := doBounded(h, c.method, c.path, "read-token"); code != http.StatusForbidden {
			t.Errorf("%s %s with read token: got %d, want 403", c.method, c.path, code)
		}
	}
}

func TestReadToken_UnlistedRoutesForbidden(t *testing.T) {
	h := newThreeTokenServer(t)
	for _, c := range unlistedRoutes {
		if code := doBounded(h, c.method, c.path, "read-token"); code != http.StatusForbidden {
			t.Errorf("%s %s with read token: got %d, want 403", c.method, c.path, code)
		}
	}
}

func TestReadToken_NoCookieAndNoLogin(t *testing.T) {
	h := newThreeTokenServer(t)

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.AddCookie(&http.Cookie{Name: cookieToken, Value: "read-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("read token as cookie on pravda: got %d, want 303 to /login", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/log/since?since=0", nil)
	req.AddCookie(&http.Cookie{Name: cookieToken, Value: "read-token"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("read token as cookie on API: got %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token=read-token"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); loc != "/login?error=1" {
		t.Errorf("login with read token: Location = %q, want /login?error=1", loc)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("login with read token minted a cookie")
	}
}

func TestReadToken_DecideRefusedInsideHandler(t *testing.T) {
	s := &Server{Store: newTestStore(t), Token: "full-token", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.WithValue(context.Background(), readRoleKey{}, true)
	body := `{"path":"CLAUDE.md","version":1,"decision":"approved"}`
	req := httptest.NewRequest(http.MethodPost, "/review/api/decide", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleReviewDecide(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("decide under read role: got %d, want 403", rec.Code)
	}
}

func TestPushToken_StillCannotReadWithReadTokenConfigured(t *testing.T) {
	h := newThreeTokenServer(t)
	for _, c := range append(append([]route{}, apiReadRoutes...), pravdaReadRoutes...) {
		if code := doBounded(h, c.method, c.path, "push-token"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with push token: got %d, want 401", c.method, c.path, code)
		}
	}
	for _, c := range reviewActions {
		if code := doBounded(h, c.method, c.path, "push-token"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with push token: got %d, want 401", c.method, c.path, code)
		}
	}
	for _, c := range writeRoutes {
		if code := doBounded(h, c.method, c.path, "push-token"); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s %s with push token: got %d, want authorized", c.method, c.path, code)
		}
	}
}

func TestFullToken_ReachesEveryClass(t *testing.T) {
	h := newThreeTokenServer(t)
	var all []route
	for _, set := range [][]route{apiReadRoutes, pravdaReadRoutes, writeRoutes, pravdaWriteRoutes, reviewActions, unlistedRoutes[:3]} {
		all = append(all, set...)
	}
	for _, c := range all {
		if code := doBounded(h, c.method, c.path, "full-token"); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s %s with full token: got %d, want authorized", c.method, c.path, code)
		}
	}
}

func TestReadToken_UnsetNeverMatches(t *testing.T) {
	s := &Server{Store: newTestStore(t), Token: "full-token", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Watch: &stubHandler{}}
	h := s.Routes()
	if code := doBounded(h, http.MethodGet, "/log/since?since=0", ""); code != http.StatusUnauthorized {
		t.Errorf("empty bearer, no read token configured: got %d, want 401", code)
	}
	if code := doBounded(h, http.MethodGet, "/log/since?since=0", "read-token"); code != http.StatusUnauthorized {
		t.Errorf("unconfigured read token: got %d, want 401", code)
	}
}
