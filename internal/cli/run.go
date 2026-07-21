package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/telemetry"
)

type Dependencies struct {
	Version string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

const rootHelp = `Recallstack CLI

Usage:
  recallstack <command> [options]

Commands:
  login [code]        Sign in using a one-time code
  logout              Sign out and clear local credentials
  whoami              Show the authenticated account
  config              Manage endpoint settings
  workspace           Inspect accessible workspaces
  project             Manage the local project target
  memory              Ingest and retrieve memories
  agent               Manage agent integrations

Options:
  -h, --help           Show help
  -V, --version        Show version
`

func Run(args []string, deps Dependencies) error {
	if deps.Stdin == nil || deps.Stdout == nil || deps.Stderr == nil {
		return fmt.Errorf("CLI streams are required")
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := io.WriteString(deps.Stdout, rootHelp)
		return err
	}
	if args[0] == "--version" || args[0] == "-V" || args[0] == "version" {
		_, err := fmt.Fprintln(deps.Stdout, deps.Version)
		return err
	}
	if hasHelpFlag(args) {
		_, err := io.WriteString(deps.Stdout, helpFor(args))
		return err
	}

	args = rewriteAliases(args)
	return runCommand(args, deps)
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func helpFor(args []string) string {
	path := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || strings.HasPrefix(arg, "-") {
			continue
		}
		path = append(path, arg)
		if len(path) == 3 {
			break
		}
	}
	if len(path) == 0 {
		return rootHelp
	}
	usage := map[string]string{
		"login":           "recallstack login [code]",
		"logout":          "recallstack logout",
		"whoami":          "recallstack whoami",
		"config":          "recallstack config <show|where|base-url>",
		"config base-url": "recallstack config base-url [url] [--clear]",
		"workspace":       "recallstack workspace <list|use|show|clear>",
		"project":         "recallstack project <show|where|clear|create|list|use|set>",
		"project clear":   "recallstack project clear [--cwd <cwd>]",
		"project create":  "recallstack project create --name <name> [--description <text>] [--workspace <workspace>]",
		"memory":          "recallstack memory <ingest|query|source>",
		"memory ingest":   "recallstack memory ingest (--file <path>|--stdin) [options]",
		"memory query":    "recallstack memory query --query <query> [options]",
		"memory source":   "recallstack memory source <ingest|query|get>",
		"agent":           "recallstack agent <model|install|uninstall|status>",
		"agent model":     "recallstack agent model [codex|claude|copilot] [model] [--clear] [--global]",
		"agent install":   "recallstack agent install <codex|claude|cursor|copilot|antigravity|all> [options]",
		"agent uninstall": "recallstack agent uninstall <codex|claude|cursor|copilot|antigravity|all> [options]",
		"agent status":    "recallstack agent status [--cwd <cwd>] [--global]",
	}
	key := strings.Join(path, " ")
	for len(path) > 0 {
		if value, ok := usage[key]; ok {
			return "Usage:\n  " + value + "\n"
		}
		path = path[:len(path)-1]
		key = strings.Join(path, " ")
	}
	return rootHelp
}

func rewriteAliases(args []string) []string {
	next := append([]string(nil), args...)
	if len(next) > 0 && next[0] == "target" {
		next[0] = "project"
	}
	if len(next) > 0 && (next[0] == "config" || next[0] == "workspace" || next[0] == "project") {
		if len(next) == 1 || (strings.HasPrefix(next[1], "-") && next[1] != "--help" && next[1] != "-h") {
			next = append(next[:1], append([]string{"show"}, next[1:]...)...)
		}
	}
	return next
}

func runCommand(args []string, deps Dependencies) error {
	if args[0] == "__hook-worker" || args[0] == "__hook-retry" {
		return runHiddenHook(args, deps)
	}
	runtime, err := newRuntimeServices(deps)
	if err != nil {
		return err
	}
	started := time.Now()
	eventName, operation := telemetryCommand(args)
	var commandErr error
	switch args[0] {
	case "login", "logout", "whoami":
		commandErr = runtime.runAuth(args)
	case "config":
		commandErr = runtime.runConfig(args[1:])
	case "workspace":
		commandErr = runtime.runWorkspace(args[1:])
	case "project":
		commandErr = runtime.runProject(args[1:])
	case "memory":
		commandErr = runtime.runMemory(args[1:])
	case "agent":
		commandErr = runtime.runAgent(args[1:])
	case "hook":
		commandErr = runtime.runHook(args[1:])
	default:
		return fmt.Errorf("unknown command %q", strings.Join(args, " "))
	}
	if eventName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		outcome := "success"
		if commandErr != nil {
			outcome = "error"
		}
		telemetry.Emit(ctx, runtime.auth, &runtime.config, telemetry.Event{
			Name: eventName, Operation: operation, Outcome: outcome, DurationMS: time.Since(started).Milliseconds(),
		})
		telemetry.EmitTrace(ctx, runtime.auth, &runtime.config, telemetry.Trace{
			TraceID: runtime.traceID, SpanID: runtime.spanID, Name: operation,
			ClientID: runtime.clientID, StartedAt: started, EndedAt: time.Now(), Outcome: outcome,
		})
	}
	return commandErr
}

func telemetryCommand(args []string) (string, string) {
	if len(args) == 0 {
		return "", ""
	}
	key := args[0]
	if len(args) > 1 {
		key += " " + args[1]
	}
	events := map[string]string{
		"login":           "cli.auth.login",
		"workspace use":   "cli.workspace.use",
		"project use":     "cli.project.use",
		"project set":     "cli.project.use",
		"memory query":    "cli.memory.query",
		"memory ingest":   "cli.memory.ingest",
		"agent install":   "cli.agent.install",
		"agent uninstall": "cli.agent.uninstall",
		"agent status":    "cli.agent.status",
	}
	if len(args) > 2 && args[0] == "memory" && args[1] == "source" {
		key = "memory source " + args[2]
		events["memory source ingest"] = "cli.memory.source.ingest"
		events["memory source query"] = "cli.memory.source.query"
		events["memory source get"] = "cli.memory.source.get"
	}
	event := events[key]
	if event == "" {
		return "", ""
	}
	return event, event
}
