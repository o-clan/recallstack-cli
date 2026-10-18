package main

import (
	"fmt"
	"os"

	"github.com/o-clan/recallstack-cli/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Run(os.Args[1:], cli.Dependencies{
		Version: version,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}); err != nil {
		code, message := cli.AsExit(err)
		if message != "" {
			fmt.Fprintln(os.Stderr, message)
		}
		os.Exit(code)
	}
}
