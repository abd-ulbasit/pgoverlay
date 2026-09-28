package main

import (
	"os"

	"github.com/abd-ulbasit/pgoverlay/internal/cli"
)

func main() {
	// ExecuteC reports which command ran: `pgb doctor` exits 1 for drift and
	// 2 when it could not compute the plan, everything else 1 on error.
	if cmd, err := cli.NewRootCmd().ExecuteC(); err != nil {
		os.Exit(cli.ExitCode(cmd, err))
	}
}
