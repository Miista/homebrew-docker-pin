package queue

import "sync"

// CheckRun is what a check a person asked for did, as the page reports it.
//
// Only the latest is kept, and only in memory: it is the answer to a click,
// read by the page that clicked, and what happened before is in the queue and
// the log. It rides on the snapshot the page already polls, so there is no
// second route to ask.
type CheckRun struct {
	// Seq numbers this host's asked checks, so a page can tell the run it
	// asked for from the one before it.
	Seq int `json:"seq"`
	// Service is what was asked about; empty for every service.
	Service string `json:"service,omitempty"`
	// Done is whether the check has ended.
	Done bool `json:"done"`
	// Error is why the whole check could not run.
	Error string `json:"error,omitempty"`
	// Checked is how many services were looked at without error.
	Checked int `json:"checked"`
	// Failed are the services that could not be looked at.
	Failed []CheckFailure `json:"failed,omitempty"`
	// Findings is what was found, and what became of each.
	Findings []CheckFinding `json:"findings,omitempty"`
}

// CheckFailure is a service a check could not look at.
type CheckFailure struct {
	Service string `json:"service"`
	Error   string `json:"error"`
}

// CheckFinding is one release a check found, and what the queue did about it.
type CheckFinding struct {
	Service string `json:"service"`
	// Label names the release: a version, or the moving tag that moved.
	Label string `json:"label,omitempty"`
	Fate  Fate   `json:"fate"`
	// Message is why, for a release skipped or rejected.
	Message string `json:"message,omitempty"`
	// Until is when a deferred release is due, RFC 3339.
	Until string `json:"until,omitempty"`
}

// CheckRecorder collects one asked check's run as it happens.
type CheckRecorder struct {
	mu  sync.Mutex
	run CheckRun
}

func newCheckRecorder(seq int, service string) *CheckRecorder {
	return &CheckRecorder{run: CheckRun{Seq: seq, Service: service}}
}

// Found records what became of one finding. A nil recorder records nothing,
// so a scheduled check, which has none, calls the same code.
func (c *CheckRecorder) Found(r Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.run.Findings = append(c.run.Findings, CheckFinding{
		Service: r.Service, Label: r.Label, Fate: r.Fate, Message: r.Message, Until: r.Until,
	})
}

// Checked records a service's check ending: nil for one looked at, the error
// for one that could not be.
func (c *CheckRecorder) Checked(service string, err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.run.Failed = append(c.run.Failed, CheckFailure{Service: service, Error: err.Error()})
		return
	}
	c.run.Checked++
}

func (c *CheckRecorder) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.run.Done = true
	if err != nil {
		c.run.Error = err.Error()
	}
}

// Run is the run so far, a copy.
func (c *CheckRecorder) Run() CheckRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.run
	r.Failed = append([]CheckFailure(nil), r.Failed...)
	r.Findings = append([]CheckFinding(nil), r.Findings...)
	return r
}
