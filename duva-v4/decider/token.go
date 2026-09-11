package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
)

// tokenFromEnv is the bearer token a UI must present, minted if none was
// given.
//
// Minted rather than left empty, because empty means "anything that can reach
// this may approve an update" -- and an approval is a container being replaced
// on this host. A default that open is not a default anyone chose; it is one
// they inherited by not reading a warning.
//
// The cost is that a minted token changes on every restart, so anything
// holding it goes stale. That is the right trade while the alternative is no
// token at all: a UI that has to be re-pointed after a restart is an
// annoyance, and an open approval endpoint is not. Inject DECIDER_TOKEN for
// anything long-lived.
func tokenFromEnv() string {
	if t := os.Getenv("DECIDER_TOKEN"); t != "" {
		return t
	}
	return mintToken()
}

// mintToken returns 32 random bytes, hex-encoded.
//
// crypto/rand, and its error is fatal rather than fallen back from: a token
// from a degraded source is worse than refusing to start, because it looks
// exactly like a good one.
func mintToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("cannot mint a token: " + err.Error())
	}
	return hex.EncodeToString(b)
}
