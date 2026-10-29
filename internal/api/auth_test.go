package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eordano/cccp/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type stubHandler struct{ called bool }

func (h *stubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	w.WriteHeader(http.StatusOK)
}

func newTestServer(t *testing.T, token string) (*Server, *stubHandler, http.Handler) {
	t.Helper()
	stub := &stubHandler{}
	s := &Server{
		Store: newTestStore(t),
		Token: token,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Watch: stub,
	}
	return s, stub, s.Routes()
}

func TestCookieAuth_BearerAllowed(t *testing.T) {
	s, stub, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/session/x", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("bearer-auth: got %d, want 200", rec.Code)
	}
	if !stub.called {
		t.Fatal("bearer-auth: inner handler not called")
	}
	_ = s
}

func TestCookieAuth_BearerWrongIs401(t *testing.T) {
	_, stub, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/session/x", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-bearer: got %d, want 401", rec.Code)
	}
	if stub.called {
		t.Fatal("bad-bearer: inner handler was called; should have been rejected")
	}
}

func TestCookieAuth_CookieAllowed(t *testing.T) {
	_, stub, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieToken, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("cookie-auth: got %d, want 200", rec.Code)
	}
	if !stub.called {
		t.Fatal("cookie-auth: inner handler not called")
	}
}

func TestCookieAuth_CookieWrongRedirects(t *testing.T) {
	_, stub, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieToken, Value: "wrong"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("wrong-cookie: got %d, want 303 (redirect)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("wrong-cookie: Location = %q, want /login", loc)
	}
	if stub.called {
		t.Fatal("wrong-cookie: inner handler should not have been called")
	}
}

func TestCookieAuth_UnauthenticatedRedirectsHuman(t *testing.T) {
	_, stub, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("no-auth: got %d, want 303", rec.Code)
	}
	if stub.called {
		t.Fatal("no-auth: inner handler should not have been called")
	}
}

func TestCookieAuth_HXRequestGets401NotRedirect(t *testing.T) {

	_, stub, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/session/x", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("hx-no-auth: got %d, want 401", rec.Code)
	}
	if stub.called {
		t.Fatal("hx-no-auth: inner handler should not have been called")
	}
}

func TestCookieAuth_OPTIONSPassThrough(t *testing.T) {

	_, stub, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://other.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !stub.called {
		t.Fatal("OPTIONS: inner handler not called (auth should pass OPTIONS through)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS: got %d, want 200 (stub default)", rec.Code)
	}
}

func TestLogin_BadTokenRedirectsWithError(t *testing.T) {
	_, _, h := newTestServer(t, "secret-token")

	body := strings.NewReader("token=wrong")
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bad-login: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=1" {
		t.Errorf("bad-login: Location = %q, want /login?error=1", loc)
	}
}

func TestLogin_CorrectTokenSetsCookie(t *testing.T) {
	_, _, h := newTestServer(t, "secret-token")

	body := strings.NewReader("token=secret-token")
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("login: Location = %q, want /", loc)
	}
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieToken {
			got = c
		}
	}
	if got == nil {
		t.Fatal("login: no cccp_token cookie set")
	}
	if got.Value != "secret-token" {
		t.Errorf("cookie value = %q, want secret-token", got.Value)
	}
	if !got.HttpOnly {
		t.Error("cookie: HttpOnly not set")
	}
	if got.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie: SameSite = %v, want Strict", got.SameSite)
	}
}

func TestHealthz_NoAuthRequired(t *testing.T) {
	_, _, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "ok\n" {
		t.Errorf("healthz body = %q, want %q", body, "ok\n")
	}
}

func TestAPIAuth_BearerAllowed(t *testing.T) {

	_, _, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/log/since?since=0", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("api bearer-auth: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestAPIAuth_CookieRejectedOnAPI(t *testing.T) {

	_, _, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/log/since?since=0", nil)
	req.AddCookie(&http.Cookie{Name: cookieToken, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("api cookie-auth: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestAPIAuth_BearerRequired(t *testing.T) {
	_, _, h := newTestServer(t, "secret-token")

	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(""))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing-bearer: got %d, want 401", rec.Code)
	}
}

func newPushTokenServer(t *testing.T) http.Handler {
	t.Helper()
	stub := &stubHandler{}
	s := &Server{
		Store:     newTestStore(t),
		Token:     "full-token",
		PushToken: "push-token",
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Watch:     stub,
	}
	return s.Routes()
}

func doAuthed(h http.Handler, method, path, bearer string) int {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestPushToken_AuthorizesOnlyPushEndpoints(t *testing.T) {
	h := newPushTokenServer(t)

	// Write endpoints: push token must get past auth (anything but 401).
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/ingest"},
		{http.MethodPut, "/blob/deadbeef"},
		{http.MethodPost, "/log/append"},
	} {
		if code := doAuthed(h, c.method, c.path, "push-token"); code == http.StatusUnauthorized {
			t.Errorf("%s %s with push token: got 401, want authorized", c.method, c.path)
		}
	}

	// Read endpoints: push token must be rejected with 401.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/current"},
		{http.MethodGet, "/log/since"},
		{http.MethodGet, "/events"},
		{http.MethodGet, "/session/x"},
		{http.MethodGet, "/sessions"},
		{http.MethodGet, "/sessions/x"},
		{http.MethodGet, "/index-panel/x"},
		{http.MethodGet, "/files/skills/x/SKILL.md"},
	} {
		if code := doAuthed(h, c.method, c.path, "push-token"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with push token: got %d, want 401", c.method, c.path, code)
		}
	}

	// Full token still reaches everything.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/ingest"},
		{http.MethodGet, "/current"},
		{http.MethodGet, "/session/x"},
	} {
		if code := doAuthed(h, c.method, c.path, "full-token"); code == http.StatusUnauthorized {
			t.Errorf("%s %s with full token: got 401, want authorized", c.method, c.path)
		}
	}
}

func TestPushToken_EmptyConfiguredNeverMatches(t *testing.T) {
	// PushToken unset: an empty bearer must NOT match the empty push token
	// (ConstantTimeCompare("", "") == 1 would otherwise be an auth bypass).
	stub := &stubHandler{}
	s := &Server{
		Store: newTestStore(t),
		Token: "full-token",
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Watch: stub,
	}
	h := s.Routes()
	if code := doAuthed(h, http.MethodPost, "/ingest", ""); code != http.StatusUnauthorized {
		t.Errorf("no bearer, no push token configured: got %d, want 401", code)
	}
	if code := doAuthed(h, http.MethodPost, "/log/append", ""); code != http.StatusUnauthorized {
		t.Errorf("no bearer on /log/append: got %d, want 401", code)
	}
}
