package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miista/authy"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/ui"
)

type emptySource struct{}

func (emptySource) Pending() []ui.Hosted      { return nil }
func (emptySource) Blocked() []ui.Blocked     { return nil }
func (emptySource) Unreachable() []ui.Problem { return nil }

// countingApprover records every approval it is asked for. The point of the
// gate is that none of them happen without a session.
type countingApprover struct{ calls int }

func (a *countingApprover) Approve(string) error { a.calls++; return nil }

func gatedHandler(t *testing.T, env map[string]string) (http.Handler, *countingApprover) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gate, err := authy.FromEnv(ctx, "duva-ui", authy.WithDataDir(filepath.Join(t.TempDir(), "auth")))
	if err != nil {
		t.Fatal(err)
	}
	ap := &countingApprover{}
	return newHandler(&ui.Server{Source: emptySource{}, Approver: ap, Version: "test"}, gate), ap
}

func serve(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// An OIDC provider nothing answers is fine here: the gate retries in the
// background and refuses everything meanwhile.
var oidcEnv = map[string]string{
	"DUVA_UI_OIDC_ISSUER":        "http://127.0.0.1:1",
	"DUVA_UI_OIDC_CLIENT_ID":     "duva-ui",
	"DUVA_UI_OIDC_CLIENT_SECRET": "secret",
	"DUVA_UI_OIDC_BASE_URL":      "https://duva.example.test",
}

func TestGateRefusesUnauthenticatedRequests(t *testing.T) {
	h, ap := gatedHandler(t, oidcEnv)

	// Every route that shows or does anything, the approval ones included.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/state"},
		{"GET", "/api/stream/optiplex/caddy"},
		{"POST", "/api/apply/optiplex/caddy"},
		{"POST", "/apply"},
		{"GET", "/logo.png"},
	} {
		rec := serve(h, tc.method, tc.path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", tc.method, tc.path, rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"unauthorized"}` {
			t.Errorf("%s %s body = %q, want the gate's JSON 401", tc.method, tc.path, got)
		}
	}
	if ap.calls != 0 {
		t.Errorf("an update was approved %d time(s) without a session", ap.calls)
	}

	rec := serve(h, "GET", "/", map[string]string{"Sec-Fetch-Mode": "navigate", "Accept": "text/html"})
	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		t.Errorf("page load without a session = %d, want a redirect", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "duva") {
		t.Error("page load without a session served the page")
	}
}

func TestGateClosesLocalSetupWhenOIDCIsConfigured(t *testing.T) {
	h, _ := gatedHandler(t, oidcEnv)
	for _, path := range []string{"/auth/setup", "/auth/local"} {
		if rec := serve(h, "GET", path, nil); rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 with OIDC configured and local login off; nobody may claim the app by being first", path)
		}
	}
}

func TestHealthzIsPublic(t *testing.T) {
	h, _ := gatedHandler(t, oidcEnv)
	if rec := serve(h, "GET", "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200 (the container healthcheck has no session)", rec.Code)
	}
}

func TestAuthDisabledServesWithoutSignIn(t *testing.T) {
	h, _ := gatedHandler(t, map[string]string{"DUVA_UI_AUTH_DISABLED": "true"})
	if rec := serve(h, "GET", "/api/state", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /api/state with the gate off = %d, want 200", rec.Code)
	}
	if rec := serve(h, "GET", "/auth/client.js", nil); rec.Code == http.StatusOK {
		t.Error("GET /auth/client.js with the gate off = 200, want it absent (keeps Sign out hidden)")
	}
}

func TestIncompleteOIDCConfigurationIsRefused(t *testing.T) {
	t.Setenv("DUVA_UI_OIDC_ISSUER", "https://auth.example.test")
	// The other three are unset: starting anyway would fall back to the local
	// account, so FromEnv has to refuse.
	if _, err := authy.FromEnv(context.Background(), "duva-ui", authy.WithDataDir(t.TempDir())); err == nil {
		t.Error("FromEnv accepted a partial OIDC configuration")
	}
}
