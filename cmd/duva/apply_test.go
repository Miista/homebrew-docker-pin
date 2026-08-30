package main

import "testing"

// The watcher skips these, so reaching pullImage means a reference that looked
// resolved when it was queued and does not now. Refused rather than attempted:
// the transaction is about to rewrite a compose file.
func TestPullImageRefusesAnUnexpandedVariable(t *testing.T) {
	pulled := false
	d := Docker{Pull: func(string) error { pulled = true; return nil }}

	if err := pullImage("nginx:${TAG}", d); err == nil {
		t.Error("a reference with a variable should not be pulled")
	}
	if pulled {
		t.Error("the daemon should not have been asked for it")
	}
}

// And the ordinary case still pulls -- a guard that refuses everything would
// pass the test above while breaking every upgrade.
func TestPullImagePullsALiteralReference(t *testing.T) {
	var got string
	d := Docker{Pull: func(ref string) error { got = ref; return nil }}

	if err := pullImage("nginx:1.25", d); err != nil {
		t.Fatalf("a literal reference should pull: %v", err)
	}
	if got != "nginx:1.25" {
		t.Errorf("pulled %q, want nginx:1.25", got)
	}
}
