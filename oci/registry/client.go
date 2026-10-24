package registry

import (
	"net"
	"net/http"
	"time"
)

// One client for every registry call in the process.
//
// Not a style preference: a fresh http.Client per call gets a fresh Transport,
// and a Transport is what holds the connection pool. Constructing one per
// operation means no connection is ever reused, so every tag listing, token
// fetch and manifest HEAD opens a new TCP connection -- and resolves the host
// again to do it.
//
// That is what makes a scheduled run look like a flood to a resolver. A check
// across ~45 services is several hundred registry operations against a handful
// of distinct hosts; with reuse that is a handful of lookups, without it, one
// per operation. Pi-hole rate-limits a client at 1000 queries a minute and
// answers the rest REFUSED, which Go surfaces as "server misbehaving" -- an
// error that reads like the registry's fault and is not.
//
// The timeout stays what each call site used, so nothing about how long a
// single request may take changes here.
var sharedClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: newTransport(),
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// The default MaxIdleConnsPerHost is 2. Registry work is bursty against
	// very few hosts -- ghcr.io, registry-1.docker.io, hub.docker.com -- so
	// the default throttles exactly the case this exists to serve, and
	// connections beyond the second are closed and redialled.
	t.MaxIdleConnsPerHost = 16
	t.MaxIdleConns = 64
	// Long enough to span one scheduled run, so a check that walks every
	// service reuses connections from the first service to the last rather
	// than redialling partway through.
	t.IdleConnTimeout = 120 * time.Second
	t.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return t
}

// httpClient is what every call site in this package uses.
//
// A function rather than the variable directly, so the call sites read the
// same as they did when each built its own, and so a future caller that needs
// its own client has an obvious place to be given one.
func httpClient() *http.Client { return sharedClient }
