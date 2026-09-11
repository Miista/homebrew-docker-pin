// duva v4: detector, decider, actor.
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
module github.com/Miista/homebrew-docker-pin/duva-v4

go 1.23

require (
	github.com/Miista/homebrew-docker-pin v0.0.0
	github.com/Miista/homebrew-docker-pin/compose v0.0.0
	github.com/Miista/homebrew-docker-pin/dockerapi v0.0.0
	github.com/Miista/homebrew-docker-pin/oci v0.0.0
	github.com/rs/zerolog v1.35.1
)

require (
	github.com/distribution/reference v0.6.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
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
