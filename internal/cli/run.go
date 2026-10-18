package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/hooks"
	"github.com/o-clan/recallstack-cli/internal/telemetry"
)

type Dependencies struct {
	Version string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// ExitError carries an exit code without printing anything more.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }

// AsExit returns the exit code an error asks for (1 by default).
func AsExit(err error) (int, string) {
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code, exit.Message
	}
	return 1, err.Error()
}

const rootHelp = `Recallstack keeps your goals, decisions and taste, and briefs every agent with them.

Usage:
  recallstack <command> [options]

Set up
  login [--no-browser]      Sign in through the browser
  logout                    Sign out on this computer
  whoami                    Show who is signed in
  agent install             Connect every agent on this computer
  agent status              Show which agents are connected
  agent uninstall <target>  Disconnect an agent
  agent model [target]      Choose the model the local extraction pass uses
  project                   Show the project for this folder
  import                    Turn rules you wrote for agents into thoughts
  brief                     Show exactly what the next session will be told
  review                    Settle what needs you, one key at a time

Thoughts
  note <kind> "<what>"      Record a goal, focus, decision, question or taste
  hold <ref> --plan "..."   Pause edits until the person answers
  resolve <ref> <choice>    Settle a hold or a conflict
  why <ref | "words">       Why something is the way it is
  adjust <ref>              Narrow, reword or re-reason a thought
  drop <ref>                End a thought
  undo [<ref>]              Reverse the latest change

Other
  config base-url [url]     Point the CLI at another API

Options:
  -h, --help           Show help
  -V, --version        Show version
`

var usage = map[string]string{
	"login":           "recallstack login [--no-browser]",
	"logout":          "recallstack logout",
	"whoami":          "recallstack whoami [--json]",
	"config":          "recallstack config <show|where|base-url>",
	"config base-url": "recallstack config base-url [url] [--clear]",
	"project":         "recallstack project [rename <name> | use <name>] [--json]",
	"project rename":  "recallstack project rename <name>",
	"project use":     "recallstack project use <name>",
	"agent":           "recallstack agent <install|status|uninstall|model>",
	"agent install":   "recallstack agent install [codex|claude|cursor|copilot|antigravity ...] [--repo] [--no-import] [--json]",
	"agent uninstall": "recallstack agent uninstall <codex|claude|cursor|copilot|antigravity|all> [--repo]",
	"agent status":    "recallstack agent status [--json]",
	"agent model":     "recallstack agent model [codex|claude|copilot] [model] [--clear]",
	"brief":           "recallstack brief [--agent <agent>] [--all] [--json]",
	"import":          "recallstack import [<file>...] [--paste] [--yes | --review | --preview] [--json]",
	"review":          "recallstack review [--all] [--json]",
	"review settle":   "recallstack review settle <id> <confirm|confirm_all|adjust|drop|a|b|neither> [--text \"<words>\"] [--where <scope>] [--why \"<why>\"] [--json]",
	"note":            "recallstack note <goal|focus|decision|question|taste> \"<what>\" [--why \"<why>\"] [--quote \"<the person's exact words>\"] [--instead-of \"<option>\"]... [--replaces <ref>] [--answers <ref>] [--where <scope>]",
	"hold":            "recallstack hold <ref> --plan \"<one line>\"",
	"resolve":         "recallstack resolve <hold-or-conflict ref> <choice> [--quote \"<the person's exact words>\"] [--text \"<one line>\"]",
	"why":             "recallstack why <ref | \"words\"> [--project <name>] [--json]",
	"adjust":          "recallstack adjust <ref> [--where <scope>] [--text \"<new wording>\"] [--why \"<reason>\"] --quote \"<the person's words>\"",
	"drop":            "recallstack drop <ref> --quote \"<the person's words>\"",
	"undo":            "recallstack undo [<ref>]",
	"hook":            "recallstack hook run <agent> <event>",
}

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
	// Hooks and the hidden workers skip config and keyring loading entirely;
	// they load it themselves only when they need the network.
	switch args[0] {
	case "hook":
		return runHook(args[1:], deps)
	case "__hook-worker", "__hook-retry", "__hook-refresh":
		return runHiddenHook(args, deps)
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
		if arg == "--" {
			return false
		}
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func helpFor(args []string) string {
	path := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		path = append(path, arg)
		if len(path) == 2 {
			break
		}
	}
	for len(path) > 0 {
		if value, ok := usage[strings.Join(path, " ")]; ok {
			return "Usage:\n  " + value + "\n"
		}
		path = path[:len(path)-1]
	}
	return rootHelp
}

func rewriteAliases(args []string) []string {
	next := append([]string(nil), args...)
	if len(next) > 0 && next[0] == "config" {
		if len(next) == 1 || (strings.HasPrefix(next[1], "-") && next[1] != "--help" && next[1] != "-h") {
			next = append(next[:1], append([]string{"show"}, next[1:]...)...)
		}
	}
	return next
}

func runCommand(args []string, deps Dependencies) error {
	r, err := newRuntimeServices(deps)
	if err != nil {
		return err
	}
	started := time.Now()
	eventName := telemetryCommand(args)
	flushes := map[string]bool{"note": true, "hold": true, "resolve": true, "why": true, "adjust": true, "drop": true, "undo": true, "brief": true, "import": true, "review": true}
	if flushes[args[0]] && r.svc.SignedIn() && len(r.svc.Queue().List("")) > 0 {
		// Writes that were waiting go first, so they land in order.
		r.svc.FlushQuietly(3 * time.Second)
	}
	var commandErr error
	switch args[0] {
	case "login", "logout", "whoami":
		commandErr = r.runAuth(args)
	case "config":
		commandErr = r.runConfig(args[1:])
	case "project":
		commandErr = r.runProject(args[1:])
	case "agent":
		commandErr = r.runAgent(args[1:])
	case "brief":
		commandErr = r.runBrief(args[1:])
	case "import":
		commandErr = r.runImport(args[1:])
	case "note":
		commandErr = r.runNote(args[1:])
	case "hold":
		commandErr = r.runHold(args[1:])
	case "resolve":
		commandErr = r.runResolve(args[1:])
	case "why":
		commandErr = r.runWhy(args[1:])
	case "adjust":
		commandErr = r.runAdjust(args[1:])
	case "drop":
		commandErr = r.runDrop(args[1:])
	case "undo":
		commandErr = r.runUndo(args[1:])
	case "review":
		commandErr = r.runReview(args[1:])
	default:
		return fmt.Errorf("unknown command %q. Run recallstack --help", strings.Join(args, " "))
	}
	if flushes[args[0]] {
		for _, record := range r.svc.Queue().List("") {
			if record.QueueReason != "auth_required" {
				hooks.SpawnRetry()
				break
			}
		}
	}
	if eventName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		outcome := "success"
		if commandErr != nil {
			outcome = "error"
		}
		cfg, cfgErr := r.svc.Config()
		if cfgErr == nil && (cfg.AccessToken != "" || cfg.APIKey != "") {
			telemetry.Emit(ctx, r.svc.Auth, &cfg, telemetry.Event{
				Name: eventName, Operation: eventName, Outcome: outcome, DurationMS: time.Since(started).Milliseconds(),
			})
		}
	}
	return commandErr
}

func telemetryCommand(args []string) string {
	key := args[0]
	if len(args) > 1 {
		key += " " + args[1]
	}
	return map[string]string{
		"login":           "cli.auth.login",
		"agent install":   "cli.agent.install",
		"agent uninstall": "cli.agent.uninstall",
	}[key]
}
