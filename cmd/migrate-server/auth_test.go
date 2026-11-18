package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestAuth builds an authenticator with fixed credentials/secret.
func newTestAuth(t *testing.T, enabled bool) *authenticator {
	t.Helper()
	t.Setenv("MIGRATE_AUTH_USER", "admin")
	t.Setenv("MIGRATE_AUTH_TTL", "1h")
	if enabled {
		t.Setenv("MIGRATE_AUTH_PASSWORD", "s3cret")
	} else {
		t.Setenv("MIGRATE_AUTH_PASSWORD", "")
	}
	return newAuthenticator()
}

func TestAuthDisabledByDefault(t *testing.T) {
	a := newTestAuth(t, false)
	if a.enabled {
		t.Fatal("auth should be disabled when password empty")
	}
	// Any request passes when auth is disabled.
	called := false
	h := a.requireAuth(func(w http.ResponseWriter, r *http.Request) { called = true })
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if !called {
		t.Fatal("handler should run when auth disabled")
	}
}

func TestLoginSuccessSetsCookieAndUnlocks(t *testing.T) {
	a := newTestAuth(t, true)

	form := url.Values{"username": {"admin"}, "password": {"s3cret"}, "next": {"/node/node-a/"}}
	req := httptest.NewRequest(http.MethodPost, "/login/submit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/node/node-a/" {
		t.Fatalf("expected redirect to next, got %q", loc)
	}
	cookies := rec.Result().Cookies()
	var session string
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("session cookie not set")
	}

	// That cookie must pass requireAuth.
	req2 := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	called := false
	a.requireAuth(func(w http.ResponseWriter, r *http.Request) { called = true })(httptest.NewRecorder(), req2)
	if !called {
		t.Fatal("valid session cookie should unlock the route")
	}
}

func TestLoginWrongPasswordRejected(t *testing.T) {
	a := newTestAuth(t, true)
	form := url.Values{"username": {"admin"}, "password": {"nope"}}
	req := httptest.NewRequest(http.MethodPost, "/login/submit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("no cookie should be set on failed login")
	}
}

func TestRequireAuthAPI401(t *testing.T) {
	a := newTestAuth(t, true)
	rec := httptest.NewRecorder()
	a.requireAuth(func(w http.ResponseWriter, r *http.Request) { t.Fatal("must not run") })(
		rec, httptest.NewRequest(http.MethodGet, "/api/workspaces", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API request should get 401, got %d", rec.Code)
	}
}

func TestRequireAuthUIRedirectsToLogin(t *testing.T) {
	a := newTestAuth(t, true)
	rec := httptest.NewRecorder()
	a.requireAuth(func(w http.ResponseWriter, r *http.Request) { t.Fatal("must not run") })(
		rec, httptest.NewRequest(http.MethodGet, "/node/node-a/", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("UI request should redirect, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/node/node-a/login?next=") {
		t.Fatalf("expected login redirect with next, got %q", loc)
	}
}

func TestSessionTamperRejected(t *testing.T) {
	a := newTestAuth(t, true)
	exp := time.Now().Add(time.Hour).Unix()
	good := a.sign(exp)
	// Flip a character to tamper the HMAC.
	bad := good[:len(good)-1] + "X"
	if a.verify(bad) {
		t.Fatal("tampered cookie must not verify")
	}
	if !a.verify(good) {
		t.Fatal("genuine cookie must verify")
	}
	// Expired cookie must not verify.
	if a.verify(a.sign(time.Now().Add(-time.Minute).Unix())) {
		t.Fatal("expired cookie must not verify")
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	a := newTestAuth(t, true)
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: a.sign(time.Now().Add(-time.Second).Unix())})
	rec := httptest.NewRecorder()
	a.requireAuth(func(w http.ResponseWriter, r *http.Request) { t.Fatal("must not run") })(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session should 401, got %d", rec.Code)
	}
}

func TestLoginThrottle(t *testing.T) {
	a := newTestAuth(t, true)
	ip := "1.2.3.4"
	for i := 0; i < loginThrottleMax; i++ {
		a.recordFailure(ip)
	}
	if !a.throttled(ip) {
		t.Fatal("IP should be throttled after max failures")
	}
	a.clearFailures(ip)
	if a.throttled(ip) {
		t.Fatal("clearFailures should reset throttle")
	}
}

func TestSafeNextBlocksOpenRedirect(t *testing.T) {
	cases := map[string]string{
		"/node/a/":   "/node/a/",
		"":           "",
		"//evil.com": "",
		"https://x":  "",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	a := newTestAuth(t, true)
	rec := httptest.NewRecorder()
	a.handleLogout(rec, httptest.NewRequest(http.MethodGet, "/logout", nil))
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout should clear the session cookie")
	}
}

// TestRouteNodeGuardsConsole ensures the per-node console UI (/node/<self>/)
// is gated, while its login paths stay open — the exact hole the routeNode
// wrapper exists to close.
func TestRouteNodeGuardsConsole(t *testing.T) {
	a := newTestAuth(t, true)
	s := &server{nodeName: "node-a", auth: a}

	// Console UI without a session must redirect to login, not render.
	rec := httptest.NewRecorder()
	s.routeNode(rec, httptest.NewRequest(http.MethodGet, "/node/node-a/", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("console UI without auth should redirect, got %d", rec.Code)
	}

	// The node-scoped login page must be reachable without a session.
	rec2 := httptest.NewRecorder()
	s.routeNode(rec2, httptest.NewRequest(http.MethodGet, "/node/node-a/login", nil))
	if rec2.Code == http.StatusSeeOther && strings.Contains(rec2.Header().Get("Location"), "/login") {
		t.Fatalf("login page must not redirect back to login, got %d %q", rec2.Code, rec2.Header().Get("Location"))
	}
}
