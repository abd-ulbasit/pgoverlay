package main

import (
	"os"

	"github.com/abd-ulbasit/pgoverlay/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
