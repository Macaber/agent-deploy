package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example/workspace-operator/internal/egress"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEgressOptionalAuthAndConflicts(t *testing.T) {
	previousEnabled, previousToken := authEnabled, expectedToken
	t.Cleanup(func() { authEnabled = previousEnabled; expectedToken = previousToken })
	t.Setenv("EGRESS_CONFIG_NAMESPACE", "test")
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := egressConfigHandler(k)
	request := func(method string, doc egressDocument, token string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(doc)
		r := httptest.NewRequest(method, "/api/egress/config", bytes.NewReader(b))
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "session_token", Value: token})
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	authEnabled = false
	c := egress.Config{Domains: []egress.Domain{{Host: "model.example.com", Port: 443}}}
	if w := request(http.MethodPut, egressDocument{Config: c}, ""); w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(http.MethodPut, egressDocument{Config: c}, ""); w.Code != http.StatusConflict {
		t.Fatal("create overwrite accepted", w.Code)
	}
	_, version, err := egress.Read(context.Background(), k, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.Domains[0].Port = 8443
	if w := request(http.MethodPut, egressDocument{Config: c, ResourceVersion: version}, ""); w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(http.MethodPut, egressDocument{Config: c, ResourceVersion: version}, ""); w.Code != http.StatusConflict {
		t.Fatal("stale write accepted", w.Code)
	}
	authEnabled = true
	expectedToken = "test-token"
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if w := request(method, egressDocument{}, ""); w.Code != http.StatusUnauthorized {
			t.Fatal("anonymous authenticated-mode access", method, w.Code)
		}
	}
	if w := request(http.MethodGet, egressDocument{}, expectedToken); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
}
