package webui

import (
	"context"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
)

func given(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

var oidc = map[string]string{
	settings.OIDCIssuer:       "https://auth.example.com",
	settings.OIDCClientID:     "duva",
	settings.OIDCClientSecret: "secret",
	settings.OIDCBaseURL:      "https://duva.example.com",
}

// With nothing configured, v4 fell back to a local account the first visitor
// created. v5 refuses: the page replaces containers and is published.
func TestNoOIDCIsARefusal(t *testing.T) {
	_, err := newGate(context.Background(), given(nil))
	if err == nil {
		t.Fatal("started without OIDC")
	}
	if !strings.Contains(err.Error(), "DUVA_SETTING_UI_OIDC_ISSUER") {
		t.Errorf("the error should name what is missing: %v", err)
	}
}

func TestPartialOIDCIsARefusal(t *testing.T) {
	vars := map[string]string{}
	for k, v := range oidc {
		vars[k] = v
	}
	delete(vars, settings.OIDCClientSecret)
	_, err := newGate(context.Background(), given(vars))
	if err == nil || !strings.Contains(err.Error(), "DUVA_SETTING_UI_OIDC_CLIENT_SECRET") {
		t.Errorf("err = %v, want one naming the missing secret", err)
	}
}
