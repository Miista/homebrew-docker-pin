package webui

import (
	"context"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

var oidc = map[string]string{
	"DUVA_UI_OIDC_ISSUER":        "https://auth.example.com",
	"DUVA_UI_OIDC_CLIENT_ID":     "duva",
	"DUVA_UI_OIDC_CLIENT_SECRET": "secret",
	"DUVA_UI_OIDC_BASE_URL":      "https://duva.example.com",
}

// With nothing configured, v4 fell back to a local account the first visitor
// created. v5 refuses: the page replaces containers and is published.
func TestNoOIDCIsARefusal(t *testing.T) {
	_, err := newGate(context.Background(), env(nil))
	if err == nil {
		t.Fatal("started without OIDC")
	}
	if !strings.Contains(err.Error(), "DUVA_UI_OIDC_ISSUER") {
		t.Errorf("the error should name what is missing: %v", err)
	}
}

func TestPartialOIDCIsARefusal(t *testing.T) {
	vars := map[string]string{}
	for k, v := range oidc {
		vars[k] = v
	}
	delete(vars, "DUVA_UI_OIDC_CLIENT_SECRET")
	_, err := newGate(context.Background(), env(vars))
	if err == nil || !strings.Contains(err.Error(), "DUVA_UI_OIDC_CLIENT_SECRET") {
		t.Errorf("err = %v, want one naming the missing secret", err)
	}
}

// A setting carried over from v4 that would weaken the gate stops the start,
// even next to a complete OIDC configuration.
func TestSettingsThatWeakenTheGateAreRefused(t *testing.T) {
	for _, gone := range []string{"DUVA_UI_AUTH_DISABLED", "DUVA_UI_LOCAL_ENABLED", "DUVA_UI_AUTH_PUBLIC_PATHS"} {
		t.Run(gone, func(t *testing.T) {
			vars := map[string]string{gone: "true"}
			for k, v := range oidc {
				vars[k] = v
			}
			_, err := newGate(context.Background(), env(vars))
			if err == nil || !strings.Contains(err.Error(), gone) {
				t.Errorf("err = %v, want a refusal naming %s", err, gone)
			}
		})
	}
}
