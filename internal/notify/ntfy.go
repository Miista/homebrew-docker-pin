// Package notify sends ntfy notifications for scheduled upgrade runs.
package notify

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Ntfy is a resolved notification target (token already looked up).
type Ntfy struct {
	URL   string
	Topic string
	Token string // empty = unauthenticated
}

// Priorities per the ntfy spec.
const (
	PriorityDefault = 3
	PriorityHigh    = 4
)

// Send publishes one message. Failures are returned, not fatal — callers
// should warn and continue, a lost notification must not fail an upgrade run.
func (n Ntfy) Send(title, message string, priority int) error {
	url := strings.TrimRight(n.URL, "/") + "/" + n.Topic
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(message))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", strconv.Itoa(priority))
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: HTTP %d publishing to %s", resp.StatusCode, url)
	}
	return nil
}
