// statequery reads duva's state file and prints one value, so the suites can
// assert on what duva recorded without depending on a JSON parser being
// installed on the host. Go is already required to build the binaries under
// test; python3 is not.
//
//	statequery <file> notified <service>   what was last announced
//	statequery <file> baseline <service>   the recorded digest for a moving tag
//	statequery <file> remembers <service>  either of the above, whichever is set
//	statequery <file> pending              queued service names, space separated
//	statequery <file> pending <svc> <key>  one field of a queued row
//
// A missing file, service or key prints nothing and exits 0: "duva recorded
// nothing" is an answer, not an error.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type state struct {
	Baseline map[string]string         `json:"baseline"`
	Notified map[string]string         `json:"notified"`
	Pending  map[string]map[string]any `json:"pending"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: statequery <file> <query> [args...]")
		os.Exit(2)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		return // no state file yet
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		fmt.Fprintf(os.Stderr, "statequery: %v\n", err)
		os.Exit(1)
	}

	switch os.Args[2] {
	case "notified":
		fmt.Print(st.Notified[arg(3)])
	case "baseline":
		fmt.Print(st.Baseline[arg(3)])
	case "remembers":
		if v := st.Notified[arg(3)]; v != "" {
			fmt.Print(v)
			return
		}
		fmt.Print(st.Baseline[arg(3)])
	case "pending":
		if len(os.Args) == 3 {
			names := make([]string, 0, len(st.Pending))
			for name := range st.Pending {
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Print(strings.Join(names, " "))
			return
		}
		row, ok := st.Pending[arg(3)]
		if !ok {
			return
		}
		if len(os.Args) == 4 {
			fmt.Print(arg(3))
			return
		}
		if v, ok := row[arg(4)]; ok {
			fmt.Print(v)
		}
	default:
		fmt.Fprintf(os.Stderr, "statequery: unknown query %q\n", os.Args[2])
		os.Exit(2)
	}
}

func arg(i int) string {
	if i < len(os.Args) {
		return os.Args[i]
	}
	return ""
}
