package main

import (
	"fmt"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
)

// runComplete implements the cobra __complete protocol the Docker CLI (v25+)
// uses to delegate shell completion to plugins: print one candidate per line,
// then a ":<directive>" line. 4 = ShellCompDirectiveNoFileComp, so the shell
// stops suggesting filenames.
func runComplete(args []string) {
	// Docker may pass the plugin command name first ("pin"); drop it.
	if len(args) > 0 && args[0] == pluginName {
		args = args[1:]
	}
	// The last word is the (possibly empty) prefix being completed.
	word := ""
	if len(args) > 0 {
		word = args[len(args)-1]
		args = args[:len(args)-1]
	}

	var candidates []string
	switch {
	case len(args) == 0:
		candidates = append([]string{"upgrade", "list", "schedule", "version", "help", "--all"}, composeServices()...)
	case args[0] == "schedule" && len(args) == 1:
		candidates = []string{"apply", "status", "remove", "run"}
	case args[0] == "schedule" && args[1] == "run":
		candidates = []string{"--dry-run"}
	case args[0] == "upgrade" && len(args) == 1:
		candidates = append([]string{"--all"}, composeServices()...)
	case args[0] == "list":
		candidates = []string{"--missing", "--quiet"}
	case args[0] == "help":
		candidates = []string{"pin", "upgrade", "list", "schedule", "version"}
	}

	for _, c := range candidates {
		if strings.HasPrefix(c, word) {
			fmt.Println(c)
		}
	}
	fmt.Println(":4")
}

// composeServices returns the services of the compose file in (or above) the
// working directory — best effort, empty on any error.
func composeServices() []string {
	composeFile, err := compose.Locate()
	if err != nil {
		return nil
	}
	services, err := compose.ListServices(composeFile)
	if err != nil {
		return nil
	}
	return services
}
