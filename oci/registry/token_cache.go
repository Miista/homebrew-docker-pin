package registry

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Bearer tokens, cached for the life of the process.
//
// This package used to discover auth per request: an unauthenticated attempt
// that earned a 401, a token fetch, then the retry. A registry like GHCR
// challenges every request, so reading one tag's publish time -- manifest,
// sub-manifest, config blob -- cost nine requests to do three operations.
//
// That is what made a scheduled check slow enough to read as a hang (96
// seconds for one tag-heavy service) and busy enough that the DNS volume
// tripped Pi-hole's rate limit. A token is valid for minutes and covers every
// request for its repository, so minting one per request was pure waste.
//
// Two things fix it together: pullToken authenticates up front, since a pull
// scope is predictable and needs no challenge to discover, and this cache
// means the second service to use a registry does not mint again.
//
// Keyed by realm+service+scope because that triple is exactly what the token
// authorises: a token minted for one repository's pull scope says nothing
// about another's, and reusing it across scopes would be a 401 loop that
// looks like a credential problem.
type tokenCache struct {
	mu sync.Mutex
	m  map[string]cachedToken
}

type cachedToken struct {
	token string
	// expires is when this stops being usable. Registries report a lifetime
	// in seconds; where one does not, the caller supplies a conservative
	// default rather than caching forever.
	expires time.Time
}

var tokens = &tokenCache{m: map[string]cachedToken{}}

// defaultTokenTTL is how long a token is trusted when the registry did not
// say. Short enough that a wrong guess costs one extra fetch, long enough to
// cover a whole run's worth of requests for one repository.
const defaultTokenTTL = 5 * time.Minute

// earlyExpiry is subtracted from a token's stated lifetime, so a token is
// never used in the last moments of its validity. A request that starts valid
// and arrives expired is a 401 the caller reports as an auth failure, which
// reads like a credential problem rather than a clock one.
const earlyExpiry = 30 * time.Second

// get returns a cached token for the key, if one is still good.
func (c *tokenCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	if !ok || !timeNow().Before(t.expires) {
		return "", false
	}
	return t.token, true
}

// put stores a token under the key. A lifetime of zero means the registry did
// not say, and the default is used.
func (c *tokenCache) put(key, token string, lifetime time.Duration) {
	if lifetime <= 0 {
		lifetime = defaultTokenTTL
	}
	if lifetime > earlyExpiry {
		lifetime -= earlyExpiry
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = cachedToken{token: token, expires: timeNow().Add(lifetime)}
}

// reset empties the cache. For tests: nothing in normal operation wants to
// throw away a token that is still valid.
func (c *tokenCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]cachedToken{}
}

// timeNow is a seam for tests, so expiry can be exercised without waiting.
var timeNow = time.Now

// pullToken returns a bearer token for pulling from one repository,
// authenticating up front rather than waiting to be challenged.
//
// A pull scope is entirely predictable -- "repository:<repo>:pull" against
// the registry's own token endpoint -- so there is nothing to learn from a
// 401 first. Discovering auth per request turned each operation into three
// round trips, and reading one tag's publish time is three operations:
// manifest, sub-manifest, config blob. Authenticating once and reusing the
// token makes that three requests instead of nine.
//
// An error is not fatal to the caller: a registry that needs no auth, or one
// whose token endpoint is elsewhere, answers the plain request fine and
// ociDo's challenge handling remains the fallback for anything this does not
// anticipate.
func pullToken(client *http.Client, baseURL, repo string) (string, bool) {
	realm := baseURL + "/token"
	service := hostOf(baseURL)
	scope := "repository:" + repo + ":pull"
	t, err := ociFetchToken(client, realm, service, scope)
	if err != nil {
		return "", false
	}
	return t, true
}

// hostOf is the host part of a base URL, which is what a registry's token
// endpoint expects as its "service".
func hostOf(baseURL string) string {
	s := strings.TrimPrefix(baseURL, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}
