package cli

import (
	"context"
	"fmt"

	"github.com/o-clan/recallstack-cli/internal/hooks"
)

func hookOptions(deps Dependencies) hooks.Options {
	return hooks.Options{Stdin: deps.Stdin, Stdout: deps.Stdout, Stderr: deps.Stderr, Version: deps.Version}
}

func runHook(args []string, deps Dependencies) error {
	if len(args) != 3 || args[0] != "run" {
		return fmt.Errorf("usage: recallstack hook run <agent> <event>")
	}
	return hooks.Run(context.Background(), args[1], args[2], hookOptions(deps))
}

func runHiddenHook(args []string, deps Dependencies) error {
	if len(args) != 3 {
		return fmt.Errorf("invalid internal hook invocation")
	}
	ctx := context.Background()
	options := hookOptions(deps)
	switch args[0] {
	case hooks.HiddenWorkerCommand():
		return hooks.Worker(ctx, args[1], args[2], options)
	case hooks.HiddenRetryCommand():
		return hooks.Retry(ctx, args[1], args[2], options)
	case hooks.HiddenRefreshCommand():
		return hooks.Refresh(ctx, args[1], args[2], options)
	}
	return fmt.Errorf("unknown internal hook command")
}
