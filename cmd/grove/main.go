package main

import (
	"fmt"
	"os"

	"github.com/gm2211/grove/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "grove:", err)
		os.Exit(1)
	}
}
