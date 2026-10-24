// The docker daemon client, as its own module.
//
// Shared: duva uses it, and so does the v4 actor -- both replace containers
// by the same means, and a second implementation of "recreate this container
// but with a different image" is exactly the drift this repo keeps splitting
// packages to avoid.
//
// It talks to the daemon's API over the mounted socket rather than shelling
// out to the docker CLI, which is why the images that use it carry no CLI.
module github.com/Miista/homebrew-docker-pin/dockerapi

go 1.23

require (
	github.com/Miista/homebrew-docker-pin/compose v0.0.0
	github.com/distribution/reference v0.6.0
)

require (
	github.com/opencontainers/go-digest v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/Miista/homebrew-docker-pin/compose => ../compose
