// The registry client, as its own module.
//
// Separate for two reasons. It is the one part of this repo that talks to
// somebody else's server, so its correctness is bounded by registries behaving
// as documented rather than by anything here -- and its tests are therefore a
// different kind of test, exercising retries, auth discovery and pagination
// against fakes rather than the logic the rest of the repo is about. Excluding
// it from the main module's `go test ./...` keeps those numbers about code
// whose behaviour is fully ours.
//
// And it makes swapping it for a library a module replacement rather than
// surgery: nothing outside imports it except the three commands that pull or
// list, and internal/pin.
//
// `version` lives here rather than in the main module because the registry
// needs it to sort tags, and a module cannot depend on the module that depends
// on it.
module github.com/Miista/homebrew-docker-pin/oci

go 1.23
