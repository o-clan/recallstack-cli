package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	hiddenWorkerCommand = "__hook-worker"
	hiddenRetryCommand  = "__hook-retry"
)

// Options contains the process surfaces used by hook commands. Zero values use
// the current process, which keeps the CLI-facing API small while allowing
// deterministic package tests.
type Options struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	CWD    string
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error
	Run    CommandRunner
	Start  SelfStarter
}

type CommandResult struct {
	Status int
	Stdout string
	Stderr string
	Err    error
}

type CommandRunner func(context.Context, string, []string, CommandOptions) CommandResult

type CommandOptions struct {
	CWD   string
	Env   []string
	Input string
}

type SelfStarter func(executable string, args []string, cwd string, env []string) (int, error)

type runtime struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	cwd    string
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	run    CommandRunner
	start  SelfStarter
}

func newRuntime(options Options) (*runtime, error) {
	cwd := options.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	rt := &runtime{
		stdin:  options.Stdin,
		stdout: options.Stdout,
		stderr: options.Stderr,
		cwd:    cwd,
		now:    options.Now,
		sleep:  options.Sleep,
		run:    options.Run,
		start:  options.Start,
	}
	if rt.stdin == nil {
		rt.stdin = os.Stdin
	}
	if rt.stdout == nil {
		rt.stdout = os.Stdout
	}
	if rt.stderr == nil {
		rt.stderr = os.Stderr
	}
	if rt.now == nil {
		rt.now = time.Now
	}
	if rt.sleep == nil {
		rt.sleep = sleepContext
	}
	if rt.run == nil {
		rt.run = runCommand
	}
	if rt.start == nil {
		rt.start = startDetached
	}
	return rt, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runCommand(ctx context.Context, command string, args []string, options CommandOptions) CommandResult {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = options.CWD
	if options.Env != nil {
		cmd.Env = options.Env
	}
	if options.Input != "" {
		cmd.Stdin = strings.NewReader(options.Input)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	status := 0
	if err != nil {
		status = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			status = exitError.ExitCode()
		}
	}
	return CommandResult{Status: status, Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
}

func (rt *runtime) nowUTC() time.Time { return rt.now().UTC() }

func (rt *runtime) envWithHookActive() []string {
	return append(os.Environ(), "RECALLSTACK_HOOK_ACTIVE=1")
}

func (rt *runtime) recallstackBinary() string {
	if configured := strings.TrimSpace(os.Getenv("RECALLSTACK_BIN")); configured != "" {
		return configured
	}
	executable, err := os.Executable()
	if err == nil {
		return executable
	}
	return "recallstack"
}

func (rt *runtime) spawnSelf(agent, mode, path, cwd string) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	command := hiddenWorkerCommand
	if mode == "retry" {
		command = hiddenRetryCommand
	}
	return rt.start(executable, []string{command, agent, path}, cwd, rt.envWithHookActive())
}

func (rt *runtime) resolveProject(ctx context.Context, cwd, stateDir string) (string, bool) {
	result := rt.run(ctx, rt.recallstackBinary(), []string{"project", "--cwd", cwd}, CommandOptions{CWD: cwd, Env: os.Environ()})
	if result.Status != 0 {
		appendError(stateDir, rt.nowUTC(), "Failed to resolve Recallstack target.", map[string]any{
			"stdout": truncate(result.Stdout, 800), "stderr": truncate(result.Stderr, 800),
		})
		return "", false
	}
	var value map[string]any
	if json.Unmarshal([]byte(result.Stdout), &value) != nil {
		return "", false
	}
	target := stringValue(value["target"])
	if target == "" || stringValue(value["project_slug"]) == "global" {
		return "", false
	}
	return target, true
}

func (rt *runtime) ingest(ctx context.Context, cwd, target, summary, idempotencyKey string, metadata map[string]any) CommandResult {
	encodedMetadata, _ := json.Marshal(metadata)
	return rt.run(ctx, rt.recallstackBinary(), []string{
		"memory", "ingest", "--project", target, "--stdin", "--idempotency-key", idempotencyKey,
		"--metadata", string(encodedMetadata),
	}, CommandOptions{CWD: cwd, Env: os.Environ(), Input: summary})
}

func envCommand(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func parsePositiveDurationMS(name string, fallback time.Duration) time.Duration {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}

type retryPolicy struct {
	Window time.Duration
	Base   time.Duration
	Max    time.Duration
}

func currentRetryPolicy() retryPolicy {
	return retryPolicy{
		Window: parsePositiveDurationMS("RECALLSTACK_INGEST_RETRY_WINDOW_MS", 24*time.Hour),
		Base:   parsePositiveDurationMS("RECALLSTACK_INGEST_RETRY_BASE_MS", 30*time.Second),
		Max:    parsePositiveDurationMS("RECALLSTACK_INGEST_RETRY_MAX_MS", 15*time.Minute),
	}
}

func (policy retryPolicy) delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := policy.Base
	for i := 1; i < attempt && delay < policy.Max; i++ {
		delay *= 2
		if delay > policy.Max {
			delay = policy.Max
		}
	}
	return delay
}

func isRetryable(result CommandResult) bool {
	combined := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	for _, fragment := range []string{
		"429", "too many requests", "rate limit", "rate-limit", "service unavailable",
		"temporarily unavailable", "bad gateway", "gateway timeout", "upstream", "fetch failed",
		"network error", "socket hang up", "connection refused", "econnrefused", "enotfound",
		"ehostunreach", "etimedout", "timed out", "timeout",
	} {
		if strings.Contains(combined, fragment) {
			return true
		}
	}
	for code := 500; code <= 599; code++ {
		if strings.Contains(combined, strconv.Itoa(code)) {
			return true
		}
	}
	return false
}

func stateDir(cwd, agent string) string {
	return filepath.Join(cwd, ".recallstack", agent+"-hooks")
}

func readPayload(reader io.Reader) (map[string]any, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return map[string]any{}, err
	}
	return value, nil
}

func normalizeAgent(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude-code", "claude_code":
		return "claude"
	case "codex":
		return "codex"
	case "cursor", "cursor-ide", "cursor_ide":
		return "cursor"
	case "copilot", "github-copilot", "github_copilot":
		return "copilot"
	case "antigravity", "google-antigravity", "google_antigravity":
		return "antigravity"
	default:
		return ""
	}
}

func adapterFor(agent string) (adapter, error) {
	switch normalizeAgent(agent) {
	case "claude":
		return claudeAdapter{}, nil
	case "codex":
		return codexAdapter{}, nil
	case "cursor":
		return cursorAdapter{}, nil
	case "copilot":
		return copilotAdapter{}, nil
	case "antigravity":
		return antigravityAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported hook agent %q", agent)
	}
}

type adapter interface {
	run(context.Context, *runtime, string, map[string]any) error
	worker(context.Context, *runtime, string) error
	retry(context.Context, *runtime, string) error
}
