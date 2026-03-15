package main

import (
	"os"

	"github.com/clankhost/clank-agent/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
