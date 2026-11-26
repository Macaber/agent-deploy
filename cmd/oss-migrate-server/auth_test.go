package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuth_DisabledWhenEmptyPassword(t *testing.T) {
	t.Setenv("OSS_MIGRATE_AUTH_PASSWORD", "")
	auth := newAuthenticator()
	if auth.enabled {
		t.Fatal("auth should be disabled when password is empty")
	}

	handler := auth.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK when auth disabled, got %d", rec.Code)
	}
}

func TestAuth_SessionWorkflow(t *testing.T) {
	t.Setenv("OSS_MIGRATE_AUTH_USER", "admin")
	t.Setenv("OSS_MIGRATE_AUTH_PASSWORD", "secret123")
	auth := newAuthenticator()
	if !auth.enabled {
		t.Fatal("auth should be enabled when password is provided")
	}

	// 1. Unauthenticated request to / should redirect to /login
	handler := auth.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("expected 302 redirect, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("expected redirect to /login, got %s", loc)
	}

	// 2. Unauthenticated API request should return 401
	apiReq := httptest.NewRequest("GET", "/api/status", nil)
	apiRec := httptest.NewRecorder()
	handler(apiRec, apiReq)
	if apiRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for API, got %d", apiRec.Code)
	}

	// 3. Login with wrong password
	badLoginReq := httptest.NewRequest("POST", "/login/submit", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	badLoginRec := httptest.NewRecorder()
	auth.handleLogin(badLoginRec, badLoginReq)
	if badLoginRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for bad password, got %d", badLoginRec.Code)
	}

	// 4. Login with correct password
	goodLoginReq := httptest.NewRequest("POST", "/login/submit", strings.NewReader(`{"username":"admin","password":"secret123"}`))
	goodLoginRec := httptest.NewRecorder()
	auth.handleLogin(goodLoginRec, goodLoginReq)
	if goodLoginRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for good login, got %d", goodLoginRec.Code)
	}

	cookies := goodLoginRec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("session cookie not found in response")
	}

	// 5. Authenticated request with session cookie
	authReq := httptest.NewRequest("GET", "/", nil)
	authReq.AddCookie(sessionCookie)
	authRec := httptest.NewRecorder()
	handler(authRec, authReq)

	if authRec.Code != http.StatusOK {
		t.Errorf("expected 200 OK with valid cookie, got %d", authRec.Code)
	}
}

func TestAuth_ForwardedPrefixRedirect(t *testing.T) {
	t.Setenv("OSS_MIGRATE_AUTH_PASSWORD", "secret123")
	auth := newAuthenticator()

	handler := auth.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Case 1: X-Forwarded-Prefix header
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/bocomwork/oss-migrate")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/bocomwork/oss-migrate/login" {
		t.Errorf("expected /bocomwork/oss-migrate/login, got %s", loc)
	}

	// Case 2: OSS_MIGRATE_URL_PREFIX env var fallback
	t.Setenv("OSS_MIGRATE_URL_PREFIX", "/bocomwork/oss-migrate")
	req2 := httptest.NewRequest("GET", "/", nil)
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)
	if loc2 := rec2.Header().Get("Location"); loc2 != "/bocomwork/oss-migrate/login" {
		t.Errorf("expected /bocomwork/oss-migrate/login via env, got %s", loc2)
	}
}
