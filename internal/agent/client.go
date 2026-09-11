package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one agent.
type Client struct {
	// BaseURL is the agent's root, e.g. http://192.0.2.10:10256.
	BaseURL string
	// Token is the bearer token the agent requires, if it requires one.
	Token string
	// HTTP is the transport. Nil uses a client with a timeout: an agent on
	// an unreachable host must fail the hub's collection in seconds, not
	// hang it until someone notices the page never loaded.
	HTTP *http.Client
}

// defaultTimeout bounds every call to an agent.
//
// A check the agent runs can take minutes, but no call here waits for one:
// Refresh starts it and returns, and progress is polled. So every request in
// this client is a small read, and anything slower than this is a host that
// is not answering rather than one that is busy.
const defaultTimeout = 10 * time.Second

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// Snapshot fetches the agent's current queue.
func (c *Client) Snapshot() (Snapshot, error) {
	var s Snapshot
	err := c.do(http.MethodGet, "/v1/snapshot", &s)
	return s, err
}

// Apply asks the agent to start updating one service. The returned error is
// either transport (the agent could not be reached) or the agent's own
// refusal, which reads the same to a caller: the update did not start.
func (c *Client) Apply(service string) error {
	var resp StartResponse
	if err := c.do(http.MethodPost, "/v1/apply/"+url.PathEscape(service), &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// Progress reports one update's state. ok is false when the agent has never
// been asked to apply this service -- or when it cannot be reached, which is
// deliberately the same answer: a hub that cannot ask has nothing to report,
// and inventing a "finished" for an update it lost track of would be worse
// than saying it does not know.
func (c *Client) Progress(service string) (ProgressResponse, bool) {
	var resp ProgressResponse
	if err := c.do(http.MethodGet, "/v1/progress/"+url.PathEscape(service), &resp); err != nil {
		return ProgressResponse{}, false
	}
	return resp, resp.Known
}

// Refresh asks the agent to check now.
func (c *Client) Refresh() error {
	var resp RefreshResponse
	if err := c.do(http.MethodPost, "/v1/refresh", &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// RefreshProgress reports the agent's in-flight check. An unreachable agent
// reports not running and not done, with the transport error as the message:
// the hub shows it rather than spinning forever on a poll that will never
// complete.
func (c *Client) RefreshProgress() (running, done bool, errMsg string) {
	var resp RefreshProgressResponse
	if err := c.do(http.MethodGet, "/v1/refresh-progress", &resp); err != nil {
		return false, true, err.Error()
	}
	return resp.Running, resp.Done, resp.Error
}

func (c *Client) do(method, path string, out any) error {
	req, err := http.NewRequest(method, strings.TrimSuffix(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body is included because an agent's refusals are readable
		// sentences, and a bare status leaves the operator guessing which of
		// several 400s they hit.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, 8<<20)); err != nil {
		return err
	}
	return json.Unmarshal(buf.Bytes(), out)
}
