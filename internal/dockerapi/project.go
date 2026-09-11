package dockerapi

import (
	"fmt"
	"os"
	"regexp"
)

// duva has to tell compose which project to act on.
//
// Left to work it out, compose derives the project name from the directory
// holding the compose file -- and duva always sees that directory at the same
// mount point, so every stack on every host would come out named after the
// mount. `compose up` would then not find the container it was asked to
// replace and would start a second one beside it.
//
// The name is not in the environment either: compose does not set
// COMPOSE_PROJECT_NAME inside the containers it starts.
//
// What is available is the container duva itself runs in. Compose labels
// every container it creates with the project, so duva reads its own labels
// and uses that. This is why duva must be a service in the stack it watches:
// its own project is the answer, which is only correct when they are the same
// project. With a root compose file that includes the rest, they are -- an
// included service still belongs to the root project.

// selfProject returns the compose project duva is running in.
type selfFuncs struct {
	// ContainerID returns the id of the container this process is in.
	ContainerID func() (string, error)
	// Label reads one label off a container.
	Label func(id, label string) (string, error)
}

var realSelf = selfFuncs{
	ContainerID: containerID,
	Label:       containerLabel,
}

// cgroupID matches the 64-hex container id docker leaves in the mount table.
var cgroupID = regexp.MustCompile(`[0-9a-f]{64}`)

// containerID finds the id of the container this process runs in.
//
// The hostname is the container id by default, but only until someone sets
// `hostname:` on the service -- then it is whatever they chose and no lookup
// finds it. The mount table carries the real id regardless, so it is tried
// first and the hostname is the fallback.
func containerID() (string, error) {
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if id := cgroupID.Find(b); id != nil {
			return string(id), nil
		}
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("finding this container's id: %w", err)
	}
	return host, nil
}

// project returns the compose project duva belongs to.
//
// An error here is fatal rather than a fallback: acting on the wrong project
// creates a duplicate container instead of replacing one, and doing that
// silently is worse than refusing. duva not being in a compose project is a
// deployment mistake, and it should say so.
func project(s selfFuncs) (string, error) {
	id, err := s.ContainerID()
	if err != nil {
		return "", err
	}
	name, err := s.Label(id, "com.docker.compose.project")
	if err != nil {
		return "", fmt.Errorf(
			"duva could not read its own compose project: %w; it must run as a service "+
				"in the stack it watches", err)
	}
	if name == "" {
		return "", fmt.Errorf(
			"duva is not running as a compose service, so it cannot tell which project " +
				"to recreate containers in; run it as a service in the stack it watches")
	}
	return name, nil
}
