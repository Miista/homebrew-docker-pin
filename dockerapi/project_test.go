package dockerapi

import (
	"errors"
	"strings"
	"testing"
)

// Reading duva's own project crosses the application boundary twice -- the
// mount table and docker -- so both are faked here. What is being tested is
// the decision made about what comes back, which is ours.

func fakeSelf(id, project string) selfFuncs {
	return selfFuncs{
		ContainerID: func() (string, error) { return id, nil },
		Label:       func(string, string) (string, error) { return project, nil },
	}
}

func TestProject_ReadsItsOwnLabel(t *testing.T) {
	got, err := project(fakeSelf("abc123", "my-real-stack"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "my-real-stack" {
		t.Errorf("got %q, want %q", got, "my-real-stack")
	}
}

// Not being in a compose project is a deployment mistake. Guessing a name
// would recreate the container in the wrong project -- which does not replace
// anything, it creates a duplicate -- so this has to fail.
func TestProject_NotAComposeServiceIsFatal(t *testing.T) {
	_, err := project(fakeSelf("abc123", ""))
	if err == nil {
		t.Fatal("expected an error when the project label is absent")
	}
	if !strings.Contains(err.Error(), "stack it watches") {
		t.Errorf("the error should say what to do about it: %v", err)
	}
}

func TestProject_ContainerIDFailureIsReported(t *testing.T) {
	boom := errors.New("no container id")
	s := selfFuncs{
		ContainerID: func() (string, error) { return "", boom },
		Label:       func(string, string) (string, error) { return "unused", nil },
	}
	if _, err := project(s); !errors.Is(err, boom) {
		t.Errorf("the underlying failure should survive: %v", err)
	}
}

func TestProject_LabelFailureIsReported(t *testing.T) {
	boom := errors.New("docker is not listening")
	s := selfFuncs{
		ContainerID: func() (string, error) { return "abc123", nil },
		Label:       func(string, string) (string, error) { return "", boom },
	}
	_, err := project(s)
	if !errors.Is(err, boom) {
		t.Errorf("the underlying failure should survive: %v", err)
	}
	if !strings.Contains(err.Error(), "its own compose project") {
		t.Errorf("the error should say what duva was trying to do: %v", err)
	}
}

// The id comes from the mount table rather than the hostname, because the
// hostname is only the container id until someone sets `hostname:` on the
// service -- and then nothing finds it. This asserts the regexp picks the id
// out of a real mountinfo line rather than the numbers around it.
func TestCgroupID_FindsTheContainerID(t *testing.T) {
	const line = "2887 2699 0:283 / /etc/hosts rw,relatime - ext4 /dev/vda1 rw " +
		"/var/lib/docker/containers/5511a8ba8d1374ae32415569b3e231ead03a1f55e2017cf6baad72d0566b1fed/hosts"
	m := cgroupID.FindStringSubmatch(line)
	want := "5511a8ba8d1374ae32415569b3e231ead03a1f55e2017cf6baad72d0566b1fed"
	if m == nil || m[1] != want {
		t.Errorf("got %q, want %q", m, want)
	}
}

// An anonymous volume is named by 64 hex characters too, and an image that
// declares VOLUME puts one in the table before the container's own files.
// Taking the first 64-hex string found the volume, and duva could not find
// itself. Lines from a real container with VOLUME ["/data"].
func TestCgroupID_SkipsAnAnonymousVolume(t *testing.T) {
	const table = "552 537 253:17 /docker/volumes/cdb15fbe5d64af9dc77e0f6ec2ef0e6107099a7ad3e6c2407119b0e08a49b0dd/_data /data rw,relatime master:134 - ext4 /dev/vdb1 rw\n" +
		"554 537 253:17 /docker/containers/e5779e30dae4bc3d7941f1423bfe13f2df3c5a5fbd63c1ac9656ba22df1082b5/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/vdb1 rw\n"
	m := cgroupID.FindStringSubmatch(table)
	want := "e5779e30dae4bc3d7941f1423bfe13f2df3c5a5fbd63c1ac9656ba22df1082b5"
	if m == nil || m[1] != want {
		t.Errorf("got %q, want the container's id %q", m, want)
	}
}
