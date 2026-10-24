package registry

import (
	"net/http"
	"strconv"
	"time"
)

// Registries throttle burst traffic (lscr.io is notorious); a scheduled run
// that lists tags for many services trips this easily. HTTP 429 is transient
// by definition, so every registry request retries a few times, honoring
// Retry-After when the server sends one.
const (
	retryAttempts     = 3
	maxRetryAfterWait = 60 * time.Second
)

// retrySleep is a seam for tests.
var retrySleep = time.Sleep

// doWithRetry runs fn, retrying on HTTP 429 with backoff. fn must be safe to
// call repeatedly (it builds its own request). The final response is returned
// even if still 429 — callers keep their normal status handling.
func doWithRetry(fn func() (*http.Response, error)) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		resp, err = fn()
		if err != nil || resp.StatusCode != http.StatusTooManyRequests {
			return resp, err
		}
		if attempt == retryAttempts-1 {
			break // out of attempts; hand back the 429
		}
		wait := time.Duration(2<<attempt) * time.Second // 2s, 4s
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
				if wait > maxRetryAfterWait {
					wait = maxRetryAfterWait
				}
			}
		}
		resp.Body.Close()
		retrySleep(wait)
	}
	return resp, err
}

// getWithRetry is doWithRetry for plain GETs.
func getWithRetry(client *http.Client, url string) (*http.Response, error) {
	return doWithRetry(func() (*http.Response, error) { return client.Get(url) })
}
