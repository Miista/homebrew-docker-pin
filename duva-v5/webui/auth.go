package webui

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/miista/authy"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
)

// authData is where the gate keeps its session-signing key: inside duva's one
// /data, rather than authy's default /app/data, which v5's image has no
// reason to carry.
const authData = "/data/auth"

// newGate builds the sign-in gate from the ui.oidc.* settings, all four of
// which are required; setting reads one by key.
//
// Not authy.FromEnv, which also offers a gate that is off and a local
// account. Neither exists here, and the variables v4 turned them on with are
// refused by name before anything starts (settings.CheckRenamed): a
// deployment carried over with DUVA_UI_AUTH_DISABLED set must fail to start,
// not quietly gain a login it did not have -- or, worse, be read as having one.
func newGate(ctx context.Context, setting func(key string) string) (*authy.Gate, error) {
	oidc := &authy.OIDCConfig{
		Issuer:       strings.TrimSpace(setting(settings.OIDCIssuer)),
		ClientID:     strings.TrimSpace(setting(settings.OIDCClientID)),
		ClientSecret: setting(settings.OIDCClientSecret),
		BaseURL:      strings.TrimSpace(setting(settings.OIDCBaseURL)),
	}
	var missing []string
	for _, v := range []struct{ key, val string }{
		{settings.OIDCIssuer, oidc.Issuer},
		{settings.OIDCClientID, oidc.ClientID},
		{settings.OIDCClientSecret, oidc.ClientSecret},
		{settings.OIDCBaseURL, oidc.BaseURL},
	} {
		if v.val == "" {
			missing = append(missing, settings.EnvVar(v.key))
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

// signedIn names the person a request is from, for the record of what they
// did: their username if the provider gave one, else their email, else the
// provider's ID for them.
func signedIn(r *http.Request) string {
	u, ok := authy.UserFrom(r)
	if !ok {
		return ""
	}
	for _, name := range []string{u.Name, u.Email, u.Subject} {
		if name != "" {
			return name
		}
	}
	return ""
}
