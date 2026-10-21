package gate

import (
	"fmt"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
)

// tokenFromEnv is the bearer token a UI must present to read or approve.
//
// Required. v4 minted one when none was given and printed it to the log, so
// the queue was never open -- but a token in `docker logs` is a credential in
// a log, readable by anything that can read the container's output, and it
// changed on every restart besides. A queue with no token configured is a
// deployment mistake to say out loud, not one to paper over.
func tokenFromEnv() (string, error) {
	t := settings.Value(settings.APIToken)
	if t == "" {
		return "", fmt.Errorf("%s is not set: it is the token a peer's page must present to read this host's queue or approve from it",
			settings.EnvVar(settings.APIToken))
	}
	return t, nil
}
