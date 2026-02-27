package main

import (
	"os"

	"github.com/anaremore/clank/apps/agent/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
