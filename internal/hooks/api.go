// Package hooks implements Recallstack's native agent hook runtime.
package hooks

import (
	"context"
	"fmt"
)

// Run reads one agent hook payload from Options.Stdin. Known hook failures are
// deliberately fail-open so memory capture can never block an agent session.
func Run(ctx context.Context, agent, event string, options Options) error {
	rt, err := newRuntime(options)
	if err != nil {
		return err
	}
	implementation, err := adapterFor(agent)
	if err != nil {
		return err
	}
	payload, payloadErr := readPayload(rt.stdin)
	if payloadErr != nil {
		payload = map[string]any{}
	}
	if err := implementation.run(ctx, rt, event, payload); err != nil {
		appendError(stateDir(rt.cwd, normalizeAgent(agent)), rt.nowUTC(), "Unhandled hook failure.", map[string]any{"error": err.Error()})
	}
	return nil
}

// Worker processes a queued hook job. It is intended for the CLI's hidden
// self-reexec command.
func Worker(ctx context.Context, agent, jobPath string, options Options) error {
	rt, err := newRuntime(options)
	if err != nil {
		return err
	}
	implementation, err := adapterFor(agent)
	if err != nil {
		return err
	}
	if jobPath == "" {
		return fmt.Errorf("hook worker job path is required")
	}
	return implementation.worker(ctx, rt, jobPath)
}

// Retry processes a queued retry record. It is intended for the CLI's hidden
// self-reexec command.
func Retry(ctx context.Context, agent, retryPath string, options Options) error {
	rt, err := newRuntime(options)
	if err != nil {
		return err
	}
	implementation, err := adapterFor(agent)
	if err != nil {
		return err
	}
	if retryPath == "" {
		return fmt.Errorf("hook retry path is required")
	}
	return implementation.retry(ctx, rt, retryPath)
}

// HiddenWorkerCommand and HiddenRetryCommand are the argv tokens used when a
// hook relaunches the current executable in the background.
func HiddenWorkerCommand() string { return hiddenWorkerCommand }
func HiddenRetryCommand() string  { return hiddenRetryCommand }
