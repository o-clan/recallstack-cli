package cli

import (
	"context"
	"fmt"

	"github.com/o-clan/recallstack-cli/internal/hooks"
)

func hookOptions(deps Dependencies, cwd string) hooks.Options {
	return hooks.Options{Stdin: deps.Stdin, Stdout: deps.Stdout, Stderr: deps.Stderr, CWD: cwd}
}

func (r *runtimeServices) runHook(args []string) error {
	if len(args) < 3 || args[0] != "run" {
		return fmt.Errorf("usage: recallstack hook run <agent> <event>")
	}
	if len(args) != 3 {
		return fmt.Errorf("hook run expects an agent and event")
	}
	return hooks.Run(context.Background(), args[1], args[2], hookOptions(r.deps, r.cwd))
}

func runHiddenHook(args []string, deps Dependencies) error {
	if len(args) != 3 {
		return fmt.Errorf("invalid internal hook invocation")
	}
	ctx := context.Background()
	options := hookOptions(deps, "")
	if args[0] == hooks.HiddenWorkerCommand() {
		return hooks.Worker(ctx, args[1], args[2], options)
	}
	if args[0] == hooks.HiddenRetryCommand() {
		return hooks.Retry(ctx, args[1], args[2], options)
	}
	return fmt.Errorf("unknown internal hook command")
}
