package main

import "testing"

func TestCheckRequest(t *testing.T) {
	for _, c := range []struct {
		args        []string
		path, since string
	}{
		{nil, "/v1/check-all", ""},
		{[]string{"app"}, "/v1/check/app", ""},
		{[]string{"app", "--since", "7d"}, "/v1/check/app", "7d"},
		{[]string{"--since", "30d", "app"}, "/v1/check/app", "30d"},
		{[]string{"--since=72h"}, "/v1/check-all", "3d"},
	} {
		path, form, err := checkRequest(c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if path != c.path || form.Get("since") != c.since {
			t.Errorf("%v = %s since %q, want %s since %q", c.args, path, form.Get("since"), c.path, c.since)
		}
	}
}

// A mistyped flag, or one in the wrong place, is refused rather than dropped:
// dropping it would run an ordinary check and say nothing.
func TestCheckRequestRefuses(t *testing.T) {
	for _, args := range [][]string{
		{"app", "--snice", "7d"},
		{"app", "db"},
		{"--since", "a week"},
		{"app", "--since", "7d", "--all"},
		{"app", "--since", "7d", "db"},
	} {
		if _, _, err := checkRequest(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
