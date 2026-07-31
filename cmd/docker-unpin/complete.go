package main

import (
	"fmt"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
)

// runComplete implements the cobra __complete protocol the Docker CLI (v25+)
// uses to delegate shell completion to plugins. ":4" = NoFileComp.
func runComplete(args []string) {
	if len(args) > 0 && args[0] == pluginName {
		args = args[1:]
	}
	word := ""
	if len(args) > 0 {
		word = args[len(args)-1]
		args = args[:len(args)-1]
	}
	var candidates []string
	if len(args) == 0 {
		candidates = append([]string{"--all", "version", "help"}, composeServices()...)
	}
	for _, c := range candidates {
		if strings.HasPrefix(c, word) {
			fmt.Println(c)
		}
	}
	fmt.Println(":4")
}

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
