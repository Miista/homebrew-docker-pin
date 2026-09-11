// Package agent is the wire between a hub and the agents it collects from.
//
// duva was one process doing two jobs: looking at a host (registry checks,
// pulling, rewriting compose files, recreating containers, committing) and
// serving a page about what it found. Those split cleanly along the machine
// boundary -- the first can only happen where the docker socket and the
// compose files are, the second can happen anywhere -- and once several hosts
// are involved the second wants to happen in one place rather than one per
// host.
//
// So an agent is duva's existing behaviour with a JSON surface instead of an
// HTML one, and a hub is the page plus a client for N agents. This package is
// what passes between them: the payload types, the server that exposes an
// agent, and the client the hub collects with.
//
// The split is deliberately not a security boundary in the direction people
// assume. An agent holds exactly what duva holds today -- the docker socket,
// the repository, write access to the compose files -- because that is what
// applying an update requires and no arrangement of processes changes it.
// What moves is that the thing serving a web page no longer needs any of it.
package agent

import (
	"github.com/Miista/homebrew-docker-pin/internal/ui"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// Snapshot is everything a hub needs from one agent to render its rows: the
// queue, the soak list, and when the agent last looked.
//
// Sent whole rather than as separate endpoints per section because they are
// one observation -- a pending list from one check and a soaking list from the
// next would be a view of a moment that never existed.
type Snapshot struct {
	// Host is the agent's name for itself. The hub trusts it for display but
	// not for routing: which agent a row came from is which agent the hub
	// asked, not what the answer claims to be. An agent misconfigured with
	// another's name would otherwise make its rows unactionable.
	Host string `json:"host"`
	// Version is the agent's build, so a hub can show when one is behind.
	Version string `json:"version"`

	Pending []watch.Pending `json:"pending"`
	Soaking []watch.Soaking `json:"soaking"`

	// LastCheck and LastCheckExact are the agent's own rendering of when it
	// last ran, rather than a timestamp for the hub to format. The agent
	// knows its own timezone and whether it has ever checked at all; a hub
	// formatting a zero time would have to reproduce both.
	LastCheck      string `json:"last_check"`
	LastCheckExact string `json:"last_check_exact"`
}

// StartResponse answers a request to apply one update. An error here means
// the transaction never began; anything it fails on afterwards arrives
// through Progress.
type StartResponse struct {
	// Error is empty when the update started. Carried in the body rather
	// than only as a status code because the reason matters to the operator
	// ("nothing queued under that name" and "one is already running" are
	// different situations) and a bare 409 does not say which.
	Error string `json:"error,omitempty"`
}

// ProgressResponse is one update's state, as the hub's poller sees it.
type ProgressResponse struct {
	// Known is false when nothing has ever been started for this service --
	// distinct from "started but no steps yet", which a caller could not
	// otherwise tell apart from "never asked".
	Known    bool        `json:"known"`
	Progress ui.Progress `json:"progress"`
}

// RefreshResponse answers a request to check now.
type RefreshResponse struct {
	// Error is empty when the check started; set when one was already
	// running.
	Error string `json:"error,omitempty"`
}

// RefreshProgressResponse reports an in-flight check.
type RefreshProgressResponse struct {
	Running bool   `json:"running"`
	Done    bool   `json:"done"`
	Error   string `json:"error,omitempty"`
}
