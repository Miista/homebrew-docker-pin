// duva v5: one binary.
//
// Its own module because it is its own thing. `docker pin` is a CLI plugin
// that rewrites an image line; this is a distributed update system -- one
// process that notices new tags, one that decides what they mean, one that
// applies the decision. They share file-format code and nothing else.
//
// The actor is the exception, and deliberately so: applying an update means
// rewriting a pin, which is docker pin's engine, so the actor depends on it
// the way any consumer would. The detector and the decider do not.
//
// Each binary has its own Dockerfile; see the Makefile here.
module github.com/Miista/homebrew-docker-pin/duva-v5

go 1.26.0

require (
	github.com/Miista/homebrew-docker-pin v0.0.0
	github.com/Miista/homebrew-docker-pin/compose v0.0.0
	github.com/Miista/homebrew-docker-pin/dockerapi v0.0.0
	github.com/Miista/homebrew-docker-pin/oci v0.0.0
	github.com/miista/authy v0.1.2
	github.com/rs/zerolog v1.35.1
)

require (
	github.com/coreos/go-oidc/v3 v3.21.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Developed together, so replaced rather than versioned: a version would mean
// tagging a release to change one line.
replace (
	github.com/Miista/homebrew-docker-pin => ..
	github.com/Miista/homebrew-docker-pin/compose => ../compose
	github.com/Miista/homebrew-docker-pin/dockerapi => ../dockerapi
	github.com/Miista/homebrew-docker-pin/oci => ../oci
)
