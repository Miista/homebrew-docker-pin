package webui

import (
	"context"
	"fmt"
	"strings"

	"github.com/miista/authy"
)

// authData is where the gate keeps its session-signing key: inside duva's one
// /data, rather than authy's default /app/data, which v5's image has no
// reason to carry.
const authData = "/data/auth"

// newGate builds the sign-in gate from DUVA_UI_OIDC_*, all four of which are
// required.
//
// Not authy.FromEnv, which also offers a gate that is off and a local
// account. Both are refused here, by name: a deployment carried over from v4
// with DUVA_UI_AUTH_DISABLED set must fail to start, not quietly gain a
// login it did not have -- or, worse, be read as having one.
func newGate(ctx context.Context, getenv func(string) string) (*authy.Gate, error) {
	for _, gone := range []string{"DUVA_UI_AUTH_DISABLED", "DUVA_UI_LOCAL_ENABLED", "DUVA_UI_AUTH_PUBLIC_PATHS"} {
		if getenv(gone) != "" {
			return nil, fmt.Errorf("%s is set, but duva v5 has no such setting: sign-in is OIDC, always", gone)
		}
	}

	oidc := &authy.OIDCConfig{
		Issuer:       strings.TrimSpace(getenv("DUVA_UI_OIDC_ISSUER")),
		ClientID:     strings.TrimSpace(getenv("DUVA_UI_OIDC_CLIENT_ID")),
		ClientSecret: getenv("DUVA_UI_OIDC_CLIENT_SECRET"),
		BaseURL:      strings.TrimSpace(getenv("DUVA_UI_OIDC_BASE_URL")),
	}
	var missing []string
	for _, v := range []struct{ name, val string }{
		{"DUVA_UI_OIDC_ISSUER", oidc.Issuer},
		{"DUVA_UI_OIDC_CLIENT_ID", oidc.ClientID},
		{"DUVA_UI_OIDC_CLIENT_SECRET", oidc.ClientSecret},
		{"DUVA_UI_OIDC_BASE_URL", oidc.BaseURL},
	} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing %s: the page is only served behind OIDC", strings.Join(missing, ", "))
	}

	local := false
	return authy.New(ctx, authy.Config{
		App:        "duva-ui",
		OIDC:       oidc,
		LocalLogin: &local,
		DataDir:    authData,
	})
}
