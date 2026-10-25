package jobs

import (
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

// How many runs in a row failed counts back from the latest to the last that
// did not, per job.
func TestFailedInARowCountsBackToTheLastSuccess(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for _, failed := range []bool{true, false, true, true, true} {
		Record(st, Run{Job: Check, Started: at, Summary: "x", Failed: failed})
	}
	Record(st, Run{Job: Sync, Started: at, Summary: "x", Failed: true})
	Record(st, Run{Job: Sync, Started: at, Summary: "x"})

	for _, s := range Statuses(st, at, at) {
		want := map[string]int{Check: 3}[s.Job]
		if s.FailedInARow != want {
			t.Errorf("%s: %d in a row, want %d", s.Job, s.FailedInARow, want)
		}
	}
}
