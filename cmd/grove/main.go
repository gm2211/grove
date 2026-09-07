package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/gm2211/grove/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		// *cli.ExitError means a command has already printed everything the user needs (e.g.
		// `grove dispatch --follow`'s "job <id> failed (exit N) on <node>" summary) and just wants
		// the process to exit with a specific code — skip the generic "grove: <err>" line, which
		// would otherwise print redundantly (and blank, since ExitError.Error() is empty).
		var exitErr *cli.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		fmt.Fprintln(os.Stderr, "grove:", err)
		os.Exit(1)
	}
}
