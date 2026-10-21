package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The portal is a pre-built Vite bundle that contains JavaScript template
// literals. It must be served verbatim instead of being parsed as an
// html/template (which rejects "{{" inside the bundle).
func TestServeHomeServesEmbeddedPortal(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	serveHome(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("unexpected content type %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", `<div id="root">`, "BocomWork"} {
		if !strings.Contains(body, want) {
			t.Errorf("portal response is missing %q", want)
		}
	}
	if len(body) < 100_000 {
		t.Errorf("portal response looks truncated: %d bytes", len(body))
	}
}

func TestServeHomeRejectsOtherPaths(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/portal.html", nil)
	rec := httptest.NewRecorder()

	serveHome(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestServeHomeSupportsETagRevalidation(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", portalETag)
	rec := httptest.NewRecorder()

	serveHome(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("expected 304 for a matching ETag, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 response must not include a body, got %d bytes", rec.Body.Len())
	}
}

func TestRequireAuthRejectsMissingSession(t *testing.T) {
	oldEnabled, oldToken := authEnabled, expectedToken
	authEnabled, expectedToken = true, "expected-token"
	defer func() {
		authEnabled, expectedToken = oldEnabled, oldToken
	}()

	called := false
	handler := requireAuth(func(http.ResponseWriter, *http.Request) {
		called = true
	})

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/api/workspaces", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a session cookie, got %d", rec.Code)
	}
	if called {
		t.Fatal("protected handler must not run without a valid session")
	}
}
