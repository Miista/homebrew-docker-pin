// Compose file reading, as its own module.
//
// Shared by both halves of this repo: the docker pin plugins, which rewrite
// image lines, and the detector/decider/actor, which read what a host
// declares. Go's internal/ is invisible across a module boundary, so a
// package two modules need cannot be internal to either.
//
// It parses YAML only to find service names, image strings and labels, and
// rewrites the image: line by line-based regex rather than re-serialising --
// so formatting and comments survive a write.
module github.com/Miista/homebrew-docker-pin/compose

go 1.23

require gopkg.in/yaml.v3 v3.0.1
