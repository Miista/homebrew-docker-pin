package update

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is how a queue reaches an update.
//
// It knows an address, a token, and the contract. It does not know what
// applying means, which is the point: a different updater at the same address
// answering the same two calls is indistinguishable from this one.
type Client struct {
	// BaseURL is the updater's root, e.g. http://actor:8080.
	BaseURL string
	// Token authenticates the queue to the update. They share a network, so
	// this is about saying who is asking rather than hiding what is said.
	Token string
	// HTTP is the transport. Nil uses a client with no timeout, because the
	// stream is long-lived by design — bounding it is the queue's job,
	// through the context it passes to Watch.
	HTTP *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{}
}

// Apply asks the updater to do the work and returns where to watch it.
func (c *Client) Apply(ctx context.Context, r Request) (Accepted, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return Accepted{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.BaseURL, "/")+"/v1/apply", bytes.NewReader(body))
	if err != nil {
		return Accepted{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	// A short timeout on starting the work, separate from the stream: an
	// updater that cannot even accept a request is not one to wait on.
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req = req.WithContext(startCtx)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Accepted{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Accepted{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var a Accepted
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return Accepted{}, fmt.Errorf("reading the actor's answer: %w", err)
	}
	if a.Stream == "" {
		return Accepted{}, fmt.Errorf("the actor accepted the work but said nowhere to watch it")
	}
	return a, nil
}

// Watch follows a stream to its end, copying every line to tee as it goes.
//
// One connection serving two readers: the queue needs to know when the work
// ended, and a person watching the UI needs to see it happen. Opening the
// stream twice would ask the updater to render the same work to two audiences,
// and give them different views of it if the second connected late.
//
// tee may be nil, for a queue applying something nobody is watching.
//
// The returned status is what the updater said. A stream that ends without
// saying anything is Failed with a reason, rather than an assumed success:
// silence is the one thing that must never read as "it worked".
func (c *Client) Watch(ctx context.Context, streamURL string, tee io.Writer) (Status, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.absolute(streamURL), nil)
	if err != nil {
		return Failed, "", err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Failed, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Failed, "", fmt.Errorf("watching: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	// Progress lines are short; a long one is a runaway rather than
	// something worth buffering megabytes for.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()

		// Relayed verbatim, terminal line included: a person watching should
		// see how it ended, not have the last line swallowed by the thing
		// that was looking for it.
		if tee != nil {
			fmt.Fprintln(tee, line)
			if f, ok := tee.(http.Flusher); ok {
				f.Flush()
			}
		}

		if status, reason, ok := ParseTerminal(line); ok {
			return status, reason, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return Failed, "", fmt.Errorf("reading the stream: %w", err)
	}

	// The updater stopped talking without saying how it went. That is a
	// failure whatever caused it -- a crash, a restart, a bug -- because the
	// alternative is calling an unfinished apply a success.
	return Failed, "the actor's stream ended without a status", nil
}

// absolute resolves a stream location against the updater's base.
//
// The updater may answer with either, and neither is wrong: a path is what it
// knows for certain, and an absolute URL is what an updater behind something
// else might need to say.
func (c *Client) absolute(loc string) string {
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	return strings.TrimSuffix(c.BaseURL, "/") + "/" + strings.TrimPrefix(loc, "/")
}

// Ready asks whether the updater would take work right now.
//
// A short timeout of its own, whatever the client carries: this is called to
// render a page, and an updater that has stopped answering must grey a button
// in a moment rather than hang the queue. Unreachable reads as not ready,
// with the transport error as the reason -- which is true, and is what an
// operator needs to see.
func (c *Client) Ready(ctx context.Context) Readiness {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/ready", nil)
	if err != nil {
		return Readiness{Reason: err.Error()}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Readiness{Reason: "the actor cannot be reached: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Readiness{Reason: "the actor answered " + resp.Status}
	}
	var out Readiness
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return Readiness{Reason: "the actor's answer could not be read: " + err.Error()}
	}
	return out
}
