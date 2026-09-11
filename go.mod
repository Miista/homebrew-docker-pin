module github.com/Miista/homebrew-docker-pin

go 1.23

require (
	github.com/distribution/reference v0.6.0 // indirect
	github.com/opencontainers/go-digest v1.0.0
	github.com/rs/zerolog v1.35.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	golang.org/x/sys v0.29.0 // indirect
)

// The registry client is a separate module -- see registry/go.mod. Replaced
// rather than required by version, because it is developed here: a version
// would mean tagging a release of it to change one line.
require github.com/Miista/homebrew-docker-pin/oci v0.0.0

replace github.com/Miista/homebrew-docker-pin/oci => ./oci

require github.com/Miista/homebrew-docker-pin/compose v0.0.0

replace github.com/Miista/homebrew-docker-pin/compose => ./compose

require github.com/Miista/homebrew-docker-pin/dockerapi v0.0.0

replace github.com/Miista/homebrew-docker-pin/dockerapi => ./dockerapi
