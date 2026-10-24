package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
)

// localQueue is a queue in the UI's own process.
type localQueue struct {
	pending  []queue.Entry
	ignored  []queue.IgnoredService
	approved []string
	refuse   string
	acted    []string
}

func (l *localQueue) Snapshot() queue.Snapshot {
	return queue.Snapshot{Host: "self", Pending: l.pending, CanApply: true, Ignored: l.ignored}
}

func (l *localQueue) Approve(service, actor string) string {
	if l.refuse != "" {
		return l.refuse
	}
	l.approved = append(l.approved, service)
	return ""
}

func (l *localQueue) ApproveEntry(id int64, actor string) string {
	l.acted = append(l.acted, fmt.Sprintf("approve entry %d by %s", id, actor))
	return ""
}

func (l *localQueue) Discard(id int64, actor string) string {
	l.acted = append(l.acted, fmt.Sprintf("discard %d by %s", id, actor))
	return ""
}

func (l *localQueue) Block(id int64, reason, actor string) string {
	l.acted = append(l.acted, fmt.Sprintf("block %d (%s) by %s", id, reason, actor))
	return ""
}

func (l *localQueue) CheckNow(service, actor string, back time.Duration) string {
	l.acted = append(l.acted, "check "+service+" "+back.String()+" by "+actor)
	return ""
}

func (l *localQueue) CheckAll(actor string, back time.Duration) string {
	l.acted = append(l.acted, "check all "+back.String()+" by "+actor)
	return l.refuse
}

func (l *localQueue) Unblock(image, digest, actor string) string {
	l.acted = append(l.acted, "unblock "+image+"@"+digest+" by "+actor)
	return ""
}

func (l *localQueue) RunJob(job, actor string) string {
	l.acted = append(l.acted, "run "+job+" by "+actor)
	return ""
}

func (l *localQueue) Stream(service string, w http.ResponseWriter, r *http.Request) error {
	fmt.Fprintln(w, "applying "+service)
	return nil
}

// The queue on the UI's own host is asked directly. Its URL is deliberately
// unreachable: anything that tried HTTP would show up as an unreachable host.
func TestALocalQueueIsAskedDirectly(t *testing.T) {
	local := &localQueue{pending: []queue.Entry{{Service: "app", From: "1.0.0", To: "1.0.1"}}}
	c := NewCollector([]Upstream{{Host: "optiplex", URL: "http://127.0.0.1:1", Local: local}})

	pending := c.Pending()
	if len(pending) != 1 || pending[0].Service != "app" || pending[0].Host != "optiplex" {
		t.Errorf("pending = %+v", pending)
	}
	if u := c.Unreachable(); len(u) != 0 {
		t.Errorf("a local queue was reported unreachable: %+v", u)
	}

	if err := c.Approve(Key("optiplex", "app"), 0, "soren"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if len(local.approved) != 1 || local.approved[0] != "app" {
		t.Errorf("approved = %v", local.approved)
	}

	rec := httptest.NewRecorder()
	if err := c.Stream(Key("optiplex", "app"), rec, httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "applying app") {
		t.Errorf("stream = %q", rec.Body.String())
	}
}

// The page asks for the queue the moment an apply's stream ends. That answer
// must be the queue after the apply, not one cached while it ran, or the
// applied row stays on screen until the next poll.
func TestAnEndedStreamIsFollowedByAFreshQueue(t *testing.T) {
	local := &localQueue{pending: []queue.Entry{{Service: "app", From: "1.0.0", To: "1.0.1"}}}
	c := NewCollector([]Upstream{{Host: "optiplex", Local: local}})
	c.MaxAge = time.Hour
	if got := c.Pending(); len(got) != 1 {
		t.Fatalf("pending = %+v", got)
	}

	local.pending = nil // the apply completes, and the stream ends
	rec := httptest.NewRecorder()
	if err := c.Stream(Key("optiplex", "app"), rec, httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if got := c.Pending(); len(got) != 0 {
		t.Errorf("the applied row is still served from the cache: %+v", got)
	}
}

// A refusal is the queue's answer, carried through as it is over HTTP.
func TestALocalRefusalIsAnError(t *testing.T) {
	local := &localQueue{refuse: "nothing is queued for app"}
	c := NewCollector([]Upstream{{Host: "optiplex", Local: local}})
	err := c.Approve(Key("optiplex", "app"), 0, "soren")
	if err == nil || !strings.Contains(err.Error(), "nothing is queued") {
		t.Errorf("err = %v", err)
	}
}

// Each host's ignored services reach the page, with the host they are on --
// for a list that only shows them.
func TestTheIgnoredServicesReachThePage(t *testing.T) {
	local := &localQueue{ignored: []queue.IgnoredService{{Service: "paperless-broker", Image: "redis", Tag: "7.4-alpine"}}}
	c := NewCollector([]Upstream{{Host: "optiplex", Local: local}})
	checks := c.LastChecks()
	if len(checks) != 1 || len(checks[0].Ignored) != 1 || checks[0].Ignored[0].Service != "paperless-broker" {
		t.Errorf("checks = %+v", checks)
	}
}
