// Package integrations connects agents to Recallstack: detection, hook
// install and removal, and status. Each agent is handled on its own, so one
// failing never stops the others, and a file Recallstack can't parse is never
// overwritten.
package integrations

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/state"
)

type Target string

const (
	TargetCodex       Target = "codex"
	TargetClaude      Target = "claude"
	TargetCursor      Target = "cursor"
	TargetCopilot     Target = "copilot"
	TargetAntigravity Target = "antigravity"
	TargetAll         Target = "all"
)

// AllTargets in the order results are printed.
var AllTargets = []Target{TargetCodex, TargetClaude, TargetCursor, TargetCopilot, TargetAntigravity}

var DisplayNames = map[Target]string{
	TargetCodex: "Codex", TargetClaude: "Claude Code", TargetCursor: "Cursor", TargetCopilot: "Copilot", TargetAntigravity: "Antigravity",
}

// Scope selects user-level (every repo) or repo-level config.
type Scope string

const (
	ScopeLocal  Scope = "local"
	ScopeGlobal Scope = "global"
)

type CommandResult struct {
	Status int
	Stdout string
	Stderr string
}

type CommandRunner func(name string, args ...string) (CommandResult, error)

type Manager struct {
	CWD        string // repo root for --repo installs
	Home       string
	Executable string
	Version    string
	Run        CommandRunner
	LookPath   func(string) (string, error)
}

func New(cwd string) (*Manager, error) {
	if strings.TrimSpace(cwd) == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	executable, _ = filepath.Abs(executable)
	return &Manager{CWD: filepath.Clean(cwd), Home: state.Home(), Executable: executable, Version: "dev", Run: runCommand, LookPath: exec.LookPath}, nil
}

func runCommand(name string, args ...string) (CommandResult, error) {
	command := exec.Command(name, args...)
	stdout, stderr := new(strings.Builder), new(strings.Builder)
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.String(), Stderr: stderr.String(), Status: -1}
	if command.ProcessState != nil {
		result.Status = command.ProcessState.ExitCode()
	}
	return result, err
}

func ParseTarget(value string) (Target, error) {
	target := Target(strings.ToLower(strings.TrimSpace(value)))
	switch target {
	case "claude-code", "claude_code":
		target = TargetClaude
	case "github-copilot":
		target = TargetCopilot
	}
	for _, known := range append(AllTargets, TargetAll) {
		if target == known {
			return target, nil
		}
	}
	return "", fmt.Errorf("unknown agent %q; use codex, claude, cursor, copilot or antigravity", value)
}

// Result is one agent's line in install and status output.
type Result struct {
	Target    Target         `json:"target"`
	Name      string         `json:"name"`
	Status    string         `json:"status"` // connected | failed | not_found
	Found     bool           `json:"found"`
	Connected bool           `json:"connected"`
	Summary   string         `json:"summary,omitempty"`
	Fix       []string       `json:"fix"`
	Error     string         `json:"error,omitempty"`
	Note      string         `json:"note,omitempty"`
	Installed map[string]any `json:"installed,omitempty"`
	// Trust is Codex's own trust state for the hooks just installed (Codex
	// runs a hook only after the person trusts it in /hooks).
	Trust *CodexTrust `json:"codex_trust,omitempty"`
}

func (m *Manager) look(name string) bool {
	lookPath := m.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	_, err := lookPath(name)
	return err == nil
}

// Found reports whether an agent is on this computer (spec section 4).
func (m *Manager) Found(target Target) bool {
	switch target {
	case TargetCodex:
		return m.look("codex") || exists(filepath.Join(m.Home, ".codex"))
	case TargetClaude:
		return m.look("claude") || exists(filepath.Join(m.Home, ".claude"))
	case TargetCursor:
		return exists(filepath.Join(m.Home, ".cursor"))
	case TargetCopilot:
		if m.look("copilot") {
			return true
		}
		for _, dir := range []string{".vscode", ".vscode-server", ".vscode-insiders"} {
			matches, _ := filepath.Glob(filepath.Join(m.Home, dir, "extensions", "github.copilot*"))
			if len(matches) > 0 {
				return true
			}
		}
		return false
	case TargetAntigravity:
		return m.look("antigravity") || exists(filepath.Join(m.Home, ".gemini", "antigravity-cli"))
	}
	return false
}

// Detect reports each agent's found and connected state without changing anything.
func (m *Manager) Detect(scope Scope) []Result {
	out := []Result{}
	for _, target := range AllTargets {
		result := Result{Target: target, Name: DisplayNames[target], Found: m.Found(target), Fix: []string{}}
		result.Connected = m.connected(target, scope)
		switch {
		case result.Connected:
			result.Status = "connected"
		case result.Found:
			result.Status = "not_connected"
		default:
			result.Status = "not_found"
		}
		out = append(out, result)
	}
	return out
}

func (m *Manager) hookCommand(target Target, event string) string {
	return quoteExecutable(m.Executable) + " hook run " + string(target) + " " + event
}

func quoteExecutable(executable string) string {
	executable = strings.TrimSpace(executable)
	if executable == "" || executable == "recallstack" {
		return "recallstack"
	}
	if runtime.GOOS == "windows" {
		return `"` + executable + `"`
	}
	if !strings.ContainsAny(executable, " '\"$`\\") {
		return executable
	}
	return "'" + strings.ReplaceAll(executable, "'", `'"'"'`) + "'"
}

// ownedCommand matches every hook command Recallstack ever installed for target.
func ownedCommand(target Target) func(string) bool {
	marker := " hook run " + string(target) + " "
	return func(command string) bool {
		command = strings.TrimSpace(command)
		if isLegacyRecallstackCommand(command) {
			return true
		}
		index := strings.LastIndex(command, marker)
		if index <= 0 || len(strings.Fields(command[index+len(marker):])) != 1 {
			return false
		}
		executable := strings.TrimSpace(command[:index])
		if len(executable) >= 2 && ((executable[0] == '\'' && executable[len(executable)-1] == '\'') || (executable[0] == '"' && executable[len(executable)-1] == '"')) {
			executable = executable[1 : len(executable)-1]
		}
		executable = strings.ReplaceAll(executable, `'"'"'`, "'")
		base := strings.ToLower(filepath.Base(executable))
		return base == "recallstack" || base == "recallstack.exe"
	}
}

// Install connects each target independently.
func (m *Manager) Install(selected []Target, scope Scope) []Result {
	out := []Result{}
	for _, target := range selected {
		result := Result{Target: target, Name: DisplayNames[target], Found: true, Fix: []string{}}
		var installed map[string]any
		var summary, note string
		var err error
		switch target {
		case TargetCodex:
			installed, summary, note, err = m.installCodex(scope)
		case TargetClaude:
			installed, summary, note, err = m.installClaude(scope)
		case TargetCursor:
			installed, summary, note, err = m.installCursor(scope)
		case TargetCopilot:
			installed, summary, note, err = m.installCopilot(scope)
		case TargetAntigravity:
			installed, summary, note, err = m.installAntigravity(scope)
		}
		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			result.Summary, result.Fix = failureLines(target, err, m.Home)
		} else {
			result.Status = "connected"
			result.Connected = true
			result.Summary = summary
			result.Note = note
			result.Installed = installed
			if target == TargetCodex {
				if trust, ok := m.CodexHooksTrust(scope); ok && !trust.Ready() {
					result.Status = "waiting_for_trust"
					result.Trust = &trust
					result.Summary, result.Fix = codexWaitingLines(trust, scope, m.Home)
					result.Note = ""
					if note != "" && !strings.HasPrefix(note, "Codex asks") {
						result.Fix = append(result.Fix, note)
					}
				} else if ok {
					result.Trust = &trust
					if strings.HasPrefix(note, "Codex asks") {
						result.Note = ""
					}
				}
			}
		}
		out = append(out, result)
	}
	return out
}

// codexWaitingLines are the install lines for Codex hooks that are written but
// that Codex won't run yet.
func codexWaitingLines(trust CodexTrust, scope Scope, home string) (string, []string) {
	path := short(trust.HooksPath, home)
	if !trust.RepoTrusted {
		return "hooks added to " + path + ", but Codex doesn't trust this folder yet", []string{
			"Codex reads a repo's .codex/ only in a trusted folder. Open Codex here and trust the folder,",
			"then run /hooks and trust the Recallstack hooks. Until then Codex skips them.",
		}
	}
	if trust.Waiting == 0 && trust.Disabled > 0 {
		return "hooks added to " + path + ", but " + countHooks(trust.Disabled) + " turned off in Codex /hooks", []string{
			"Open Codex, run /hooks and turn the Recallstack hooks back on.",
		}
	}
	lines := []string{
		"Codex skips new hooks until you trust them. Open Codex, run /hooks and trust " + countRecallstackHooks(trust.Waiting) + ".",
		"Until then Codex sessions get no brief and the hard stop does nothing.",
	}
	return "hooks added to " + path + ", waiting for you to trust them in Codex /hooks", lines
}

func countHooks(n int) string {
	if n == 1 {
		return "1 is"
	}
	return fmt.Sprintf("%d are", n)
}

func countRecallstackHooks(n int) string {
	if n == 1 {
		return "the Recallstack hook"
	}
	return fmt.Sprintf("the %d Recallstack hooks", n)
}

func failureLines(target Target, err error, home string) (string, []string) {
	var parse *ParseError
	if errors.As(err, &parse) {
		path := short(parse.Path, home)
		line := fmt.Sprintf("%s isn't valid %s", path, parse.Format)
		if parse.Line > 0 {
			line = fmt.Sprintf("%s isn't valid %s (line %d)", path, parse.Format, parse.Line)
		}
		return line + ", so it was left unchanged", []string{
			"Fix it, or move it aside:  mv " + path + " " + path + ".bak",
			"Then run:  recallstack agent install " + string(target),
		}
	}
	var write *WriteError
	if errors.As(err, &write) {
		return "couldn't write " + short(write.Path, home), []string{
			"Check the permissions on " + short(write.Path, home) + ", then run recallstack agent install " + string(target),
		}
	}
	var copilot *copilotError
	if errors.As(err, &copilot) {
		return copilot.detail, []string{"Then run:  recallstack agent install copilot"}
	}
	return err.Error(), []string{"Then run:  recallstack agent install " + string(target)}
}

func short(path, home string) string {
	if home != "" && strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// backupNote is the line for a freshly written file whose old copy was moved aside.
func backupNote(path string, existedBefore bool, home string) string {
	if existedBefore || !exists(path+".bak") {
		return ""
	}
	return "Your old file is still at " + short(path, home) + ".bak; copy back any hooks you need."
}

func (m *Manager) root(scope Scope, folder string) string {
	if scope == ScopeGlobal {
		return filepath.Join(m.Home, folder)
	}
	return filepath.Join(m.CWD, folder)
}

// ---- Claude Code ----

func (m *Manager) claudeSettingsPath(scope Scope) string {
	if scope == ScopeGlobal {
		return filepath.Join(m.Home, ".claude", "settings.json")
	}
	return filepath.Join(m.CWD, ".claude", "settings.local.json")
}

var claudeEvents = []struct{ event, arg, matcher string }{
	{"SessionStart", "session_start", ""},
	{"UserPromptSubmit", "user_prompt_submit", ""},
	{"PreToolUse", "pre_tool_use", "Edit|Write|MultiEdit|NotebookEdit|Bash|mcp__.*"},
	{"Stop", "stop", ""},
	{"SessionEnd", "session_end", ""},
}

func (m *Manager) installClaude(scope Scope) (map[string]any, string, string, error) {
	root := m.root(scope, ".claude")
	path := m.claudeSettingsPath(scope)
	settings, existed, err := readJSON(path)
	if err != nil {
		return nil, "", "", err
	}
	hooks := asObject(settings["hooks"])
	owned := ownedCommand(TargetClaude)
	for event := range hooks {
		entries, changed := removeCommands(asSlice(hooks[event]), owned, "command")
		if changed && len(entries) == 0 {
			delete(hooks, event)
		} else if changed {
			hooks[event] = entries
		}
	}
	for _, item := range claudeEvents {
		hook := object{"type": "command", "command": m.hookCommand(TargetClaude, item.arg), "timeout": float64(10)}
		if item.event == "Stop" {
			hook["async"] = true
			hook["timeout"] = float64(30)
		}
		entry := object{"hooks": []any{hook}}
		if item.matcher != "" {
			entry["matcher"] = item.matcher
		}
		hooks[item.event] = append(asSlice(hooks[item.event]), entry)
	}
	settings["hooks"] = hooks
	if err := writeJSON(path, settings); err != nil {
		return nil, "", "", err
	}
	skill := filepath.Join(root, "skills", "recallstack", "SKILL.md")
	if err := writeFile(skill, Skill, 0o644); err != nil {
		return nil, "", "", err
	}
	_, _ = remove(filepath.Join(root, "skills", "recallstack-memory.md"))
	_, _ = remove(filepath.Join(root, "skills", "recallstack-memory"))
	_, _ = remove(filepath.Join(root, "hooks", "recallstack.mjs"))
	_, _ = remove(filepath.Join(root, "hooks", "recallstack.sh"))
	return map[string]any{"settings_path": path, "skill_path": skill, "scope": scope},
		"hooks added to " + short(path, m.Home), backupNote(path, existed, m.Home), nil
}

// ---- Codex ----

var codexEvents = []struct{ event, arg, matcher string }{
	{"SessionStart", "session_start", "startup|resume|clear"},
	{"UserPromptSubmit", "user_prompt_submit", ""},
	{"PreToolUse", "pre_tool_use", ""},
	{"Stop", "stop", ""},
	{"SessionEnd", "session_end", ""},
}

func (m *Manager) installCodex(scope Scope) (map[string]any, string, string, error) {
	root := m.root(scope, ".codex")
	hooksPath := filepath.Join(root, "hooks.json")
	configPath := filepath.Join(root, "config.toml")
	config, existed, err := readJSON(hooksPath)
	if err != nil {
		return nil, "", "", err
	}
	toml, err := readText(configPath)
	if err != nil {
		return nil, "", "", err
	}
	hooks := asObject(config["hooks"])
	owned := ownedCommand(TargetCodex)
	for event := range hooks {
		entries, changed := removeCommands(asSlice(hooks[event]), owned, "command")
		if changed && len(entries) == 0 {
			delete(hooks, event)
		} else if changed {
			hooks[event] = entries
		}
	}
	for _, item := range codexEvents {
		timeout := float64(10)
		if item.event == "SessionEnd" {
			timeout = 3 // Codex clamps SessionEnd hooks to 3 s.
		}
		entry := object{"hooks": []any{object{"type": "command", "command": m.hookCommand(TargetCodex, item.arg), "timeout": timeout}}}
		if item.matcher != "" {
			entry["matcher"] = item.matcher
		}
		hooks[item.event] = append(asSlice(hooks[item.event]), entry)
	}
	config["hooks"] = hooks
	if err := writeJSON(hooksPath, config); err != nil {
		return nil, "", "", err
	}
	// Codex 0.162 calls the flag [features].hooks (codex_hooks is a deprecated
	// alias that prints a warning every run). Hooks are on by default there,
	// but an explicit flag keeps them on wherever the default differs.
	next := toml
	if tomlKeyEnabled(next, "features", "codex_hooks", "true") {
		next = removeTomlKey(next, "features", "codex_hooks")
	}
	if !tomlKeyEnabled(next, "features", "hooks", "true") {
		next = upsertTomlKey(next, "features", "hooks", "true")
	}
	if next != toml {
		if err := writeFile(configPath, []byte(next), 0o644); err != nil {
			return nil, "", "", err
		}
	}
	skill := filepath.Join(root, "skills", "recallstack", "SKILL.md")
	if err := writeFile(skill, Skill, 0o644); err != nil {
		return nil, "", "", err
	}
	_, _ = remove(filepath.Join(root, "skills", "recallstack-memory"))
	_, _ = remove(filepath.Join(root, "hooks", "recallstack.mjs"))
	note := backupNote(hooksPath, existed, m.Home)
	if note == "" {
		note = "Codex asks you to review new hooks once: open Codex, run /hooks and trust them."
	}
	return map[string]any{"hooks_config_path": hooksPath, "config_path": configPath, "skill_path": skill, "scope": scope},
		"hooks added to " + short(hooksPath, m.Home) + ", hooks turned on in " + short(configPath, m.Home), note, nil
}

// ---- Cursor ----

var cursorEvents = []string{"beforeSubmitPrompt", "afterAgentResponse", "stop", "beforeShellExecution"}

func (m *Manager) installCursor(scope Scope) (map[string]any, string, string, error) {
	root := m.root(scope, ".cursor")
	path := filepath.Join(root, "hooks.json")
	config, existed, err := readJSON(path)
	if err != nil {
		return nil, "", "", err
	}
	config["version"] = float64(1)
	hooks := asObject(config["hooks"])
	owned := ownedCommand(TargetCursor)
	for event := range hooks {
		entries, changed := removeCommands(asSlice(hooks[event]), owned, "command")
		if changed && len(entries) == 0 {
			delete(hooks, event)
		} else if changed {
			hooks[event] = entries
		}
	}
	for _, event := range cursorEvents {
		hooks[event] = append(asSlice(hooks[event]), object{"command": m.hookCommand(TargetCursor, event)})
	}
	config["hooks"] = hooks
	if err := writeJSON(path, config); err != nil {
		return nil, "", "", err
	}
	_, _ = remove(filepath.Join(root, "rules", "recallstack-memory.mdc"))
	_, _ = remove(filepath.Join(root, "hooks", "recallstack.mjs"))
	return map[string]any{"hooks_config_path": path, "scope": scope, "brief_rule": ".cursor/rules/recallstack-brief.mdc (written in each repo at stop)"},
		"hooks added to " + short(path, m.Home), backupNote(path, existed, m.Home), nil
}

// ---- Copilot ----

type copilotError struct{ detail string }

func (e *copilotError) Error() string { return e.detail }

var copilotCLIEvents = []string{"sessionStart", "userPromptSubmitted", "preToolUse", "sessionEnd"}
var copilotVSCodeEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop"}

func (m *Manager) copilotPluginRoot() string { return filepath.Join(state.Root(), "copilot-plugin") }

func (m *Manager) copilotHooks(path string) error {
	config, _, err := readJSON(path)
	if err != nil {
		return err
	}
	config["version"] = float64(1)
	hooks := asObject(config["hooks"])
	owned := ownedCommand(TargetCopilot)
	for event := range hooks {
		entries, _ := removeCommands(asSlice(hooks[event]), owned, "bash")
		entries, _ = removeCommands(entries, owned, "command")
		if len(entries) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = entries
		}
	}
	for _, event := range copilotCLIEvents {
		hooks[event] = append(asSlice(hooks[event]), object{"type": "command", "bash": m.hookCommand(TargetCopilot, event), "timeoutSec": float64(10)})
	}
	for _, event := range copilotVSCodeEvents {
		hooks[event] = append(asSlice(hooks[event]), object{"type": "command", "command": m.hookCommand(TargetCopilot, event), "timeout": float64(10)})
	}
	config["hooks"] = hooks
	return writeJSON(path, config)
}

func (m *Manager) installCopilot(scope Scope) (map[string]any, string, string, error) {
	if scope == ScopeLocal {
		path := filepath.Join(m.CWD, ".github", "hooks", "recallstack.json")
		if err := m.copilotHooks(path); err != nil {
			return nil, "", "", err
		}
		return map[string]any{"hooks_config_path": path, "scope": scope}, "hooks added to " + short(path, m.Home), "", nil
	}
	root := m.copilotPluginRoot()
	if err := m.copilotHooks(filepath.Join(root, "hooks.json")); err != nil {
		return nil, "", "", err
	}
	if err := writeFile(filepath.Join(root, "skills", "recallstack", "SKILL.md"), Skill, 0o644); err != nil {
		return nil, "", "", err
	}
	if err := writeJSON(filepath.Join(root, "plugin.json"), object{
		"name": "recallstack", "version": m.Version, "description": "Recallstack brief, hard stop and capture.",
		"skills": []any{"skills/"}, "hooks": "hooks.json",
	}); err != nil {
		return nil, "", "", err
	}
	configDir := filepath.Join(m.Home, ".copilot")
	_, _ = m.Run("copilot", "plugin", "uninstall", "recallstack", "--config-dir", configDir)
	installed, installErr := m.Run("copilot", "plugin", "install", root, "--config-dir", configDir)
	if installErr != nil || installed.Status != 0 {
		detail := strings.TrimSpace(installed.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(installed.Stdout)
		}
		if detail == "" && installErr != nil {
			detail = installErr.Error()
		}
		_, _ = remove(root)
		return nil, "", "", &copilotError{detail: "copilot plugin install failed: " + truncateLine(detail, 160)}
	}
	return map[string]any{"plugin_root": root, "config_dir": configDir, "scope": scope},
		"plugin installed from " + short(root, m.Home), "", nil
}

func truncateLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > limit {
		return value[:limit-1] + "…"
	}
	return value
}

// ---- Antigravity ----

func (m *Manager) antigravityRoot(scope Scope) string {
	if scope == ScopeGlobal {
		return filepath.Join(m.Home, ".gemini", "antigravity-cli", "plugins", "recallstack")
	}
	return filepath.Join(m.CWD, ".agents")
}

func (m *Manager) installAntigravity(scope Scope) (map[string]any, string, string, error) {
	root := m.antigravityRoot(scope)
	path := filepath.Join(root, "hooks.json")
	config, _, err := readJSON(path)
	if err != nil {
		return nil, "", "", err
	}
	plugin := asObject(config["recallstack"])
	entries, _ := removeCommands(asSlice(plugin["Stop"]), ownedCommand(TargetAntigravity), "command")
	plugin["enabled"] = true
	plugin["Stop"] = append(entries, object{"type": "command", "command": m.hookCommand(TargetAntigravity, "stop"), "timeout": float64(10)})
	config["recallstack"] = plugin
	if err := writeJSON(path, config); err != nil {
		return nil, "", "", err
	}
	skill := filepath.Join(root, "skills", "recallstack", "SKILL.md")
	if err := writeFile(skill, Skill, 0o644); err != nil {
		return nil, "", "", err
	}
	_, _ = remove(filepath.Join(root, "skills", "recallstack-memory"))
	if scope == ScopeGlobal {
		if err := writeJSON(filepath.Join(root, "plugin.json"), object{
			"name": "recallstack", "version": m.Version, "description": "Recallstack brief and capture.",
			"skills": []any{"skills/"}, "hooks": "hooks.json",
		}); err != nil {
			return nil, "", "", err
		}
	}
	return map[string]any{"root": root, "hooks_config_path": path, "skill_path": skill, "scope": scope},
		"plugin added to " + short(root, m.Home), "", nil
}

// ---- connected / uninstall ----

// pruneStop: repo installs may remove their now-empty folder; user installs
// never remove the agent's own folder.
func (m *Manager) pruneStop(scope Scope, root string) string {
	if scope == ScopeLocal {
		return m.CWD
	}
	return root
}

// Connected reports whether Recallstack's hooks are in target's config.
func (m *Manager) Connected(target Target, scope Scope) bool { return m.connected(target, scope) }

func (m *Manager) connected(target Target, scope Scope) bool {
	check := func(path string, events []string, field string) bool {
		config, _, err := readJSON(path)
		if err != nil {
			return false
		}
		hooks := asObject(config["hooks"])
		for _, event := range events {
			if !entriesContain(asSlice(hooks[event]), field, ownedCommand(target)) {
				return false
			}
		}
		return true
	}
	switch target {
	case TargetClaude:
		return check(m.claudeSettingsPath(scope), []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop", "SessionEnd"}, "command")
	case TargetCodex:
		return check(filepath.Join(m.root(scope, ".codex"), "hooks.json"), []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop", "SessionEnd"}, "command")
	case TargetCursor:
		return check(filepath.Join(m.root(scope, ".cursor"), "hooks.json"), cursorEvents, "command")
	case TargetCopilot:
		path := filepath.Join(m.copilotPluginRoot(), "hooks.json")
		if scope == ScopeLocal {
			path = filepath.Join(m.CWD, ".github", "hooks", "recallstack.json")
		}
		return check(path, copilotCLIEvents, "bash")
	case TargetAntigravity:
		config, _, err := readJSON(filepath.Join(m.antigravityRoot(scope), "hooks.json"))
		if err != nil {
			return false
		}
		plugin := asObject(config["recallstack"])
		return plugin["enabled"] == true && entriesContain(asSlice(plugin["Stop"]), "command", ownedCommand(TargetAntigravity))
	}
	return false
}

// Uninstall removes Recallstack's hooks and skills and leaves the rest alone.
func (m *Manager) Uninstall(target Target, scope Scope) (map[string]any, error) {
	switch target {
	case TargetClaude:
		root := m.root(scope, ".claude")
		updated, err := removeHooks(m.claudeSettingsPath(scope), "hooks", "command", ownedCommand(TargetClaude), scope == ScopeLocal)
		if err != nil {
			return nil, err
		}
		skillRemoved, _ := remove(filepath.Join(root, "skills", "recallstack"))
		_, _ = remove(filepath.Join(root, "skills", "recallstack-memory.md"))
		pruneEmpty([]string{filepath.Join(root, "skills")}, m.pruneStop(scope, root))
		return map[string]any{"settings_updated": updated, "skill_removed": skillRemoved}, nil
	case TargetCodex:
		root := m.root(scope, ".codex")
		hooksPath := filepath.Join(root, "hooks.json")
		updated, err := removeHooks(hooksPath, "hooks", "command", ownedCommand(TargetCodex), true)
		if err != nil {
			return nil, err
		}
		tomlUpdated := false
		if !exists(hooksPath) {
			// No other hooks left: turn the feature flag we added back off.
			configPath := filepath.Join(root, "config.toml")
			if current, err := readText(configPath); err == nil && current != "" {
				if next := removeTomlKey(removeTomlKey(current, "features", "codex_hooks"), "features", "hooks"); next != current {
					if strings.TrimSpace(next) == "" {
						_, _ = remove(configPath)
					} else if err := writeFile(configPath, []byte(next), 0o644); err != nil {
						return nil, err
					}
					tomlUpdated = true
				}
			}
		}
		skillRemoved, _ := remove(filepath.Join(root, "skills", "recallstack"))
		pruneEmpty([]string{filepath.Join(root, "skills")}, m.pruneStop(scope, root))
		return map[string]any{"hooks_updated": updated, "config_updated": tomlUpdated, "skill_removed": skillRemoved}, nil
	case TargetCursor:
		updated, err := removeHooks(filepath.Join(m.root(scope, ".cursor"), "hooks.json"), "hooks", "command", ownedCommand(TargetCursor), true)
		return map[string]any{"hooks_updated": updated}, err
	case TargetCopilot:
		if scope == ScopeLocal {
			removed, err := remove(filepath.Join(m.CWD, ".github", "hooks", "recallstack.json"))
			return map[string]any{"hooks_removed": removed}, err
		}
		result, _ := m.Run("copilot", "plugin", "uninstall", "recallstack", "--config-dir", filepath.Join(m.Home, ".copilot"))
		removed, err := remove(m.copilotPluginRoot())
		return map[string]any{"plugin_removed": removed, "uninstall_status": result.Status}, err
	case TargetAntigravity:
		root := m.antigravityRoot(scope)
		if scope == ScopeGlobal {
			removed, err := remove(root)
			pruneEmpty([]string{filepath.Dir(root)}, filepath.Join(m.Home, ".gemini", "antigravity-cli"))
			return map[string]any{"plugin_removed": removed}, err
		}
		config, _, err := readJSON(filepath.Join(root, "hooks.json"))
		if err != nil {
			return nil, err
		}
		delete(config, "recallstack")
		if len(config) == 0 {
			_, _ = remove(filepath.Join(root, "hooks.json"))
		} else if err := writeJSON(filepath.Join(root, "hooks.json"), config); err != nil {
			return nil, err
		}
		_, _ = remove(filepath.Join(root, "skills", "recallstack"))
		pruneEmpty([]string{filepath.Join(root, "skills")}, m.CWD)
		return map[string]any{"hooks_updated": true}, nil
	}
	return nil, fmt.Errorf("unknown agent %q", target)
}

// removeHooks strips owned entries from every event. With dropEmpty, a file
// left with nothing but a version number is removed.
func removeHooks(path, rootKey, field string, owned func(string) bool, dropEmpty bool) (bool, error) {
	if !exists(path) {
		return false, nil
	}
	config, _, err := readJSON(path)
	if err != nil {
		return false, err
	}
	hooks := asObject(config[rootKey])
	updated := false
	for event := range hooks {
		entries, changed := removeCommands(asSlice(hooks[event]), owned, field)
		if !changed {
			continue
		}
		updated = true
		if len(entries) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = entries
		}
	}
	if !updated {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(config, rootKey)
	} else {
		config[rootKey] = hooks
	}
	if dropEmpty && (len(config) == 0 || (len(config) == 1 && config["version"] != nil)) {
		_, err := remove(path)
		return true, err
	}
	return true, writeJSON(path, config)
}

// ---- worker models (agent model) ----

func ModelTargets() []Target { return []Target{TargetCodex, TargetClaude, TargetCopilot} }

func readSettings() object {
	settings, _, err := readJSON(state.AgentSettingsPath())
	if err != nil || settings == nil {
		return object{}
	}
	return settings
}

func WorkerModel(target Target) string {
	return strings.TrimSpace(stringField(readSettings()[string(target)], "worker_model"))
}

func SetWorkerModel(target Target, model string) error {
	settings := readSettings()
	settings[string(target)] = object{"worker_model": strings.TrimSpace(model)}
	return writeJSON(state.AgentSettingsPath(), settings)
}

func ClearWorkerModel(target Target) (bool, error) {
	settings := readSettings()
	if _, ok := settings[string(target)]; !ok {
		return false, nil
	}
	delete(settings, string(target))
	if len(settings) == 0 {
		_, err := remove(state.AgentSettingsPath())
		return true, err
	}
	return true, writeJSON(state.AgentSettingsPath(), settings)
}

// LastCapture is the newest capture time for an agent, from local state.
func LastCapture(target Target) time.Time {
	entries, err := os.ReadDir(state.CaptureDir())
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), string(target)+"-") {
			continue
		}
		value, _, err := readJSON(filepath.Join(state.CaptureDir(), entry.Name()))
		if err != nil {
			continue
		}
		for _, key := range []string{"last_sent_at", "last_capture_at"} {
			if at, err := time.Parse(time.RFC3339, stringField(value, key)); err == nil && at.After(newest) {
				newest = at
			}
		}
	}
	return newest
}

// BriefDelivery is how the agent gets its brief.
func BriefDelivery(target Target) string {
	switch target {
	case TargetCursor:
		return "brief via rule file"
	case TargetAntigravity:
		return "brief via skill file"
	}
	return "brief at session start"
}

// SortTargets keeps the canonical order.
func SortTargets(values []Target) []Target {
	order := map[Target]int{}
	for index, target := range AllTargets {
		order[target] = index
	}
	sort.SliceStable(values, func(i, j int) bool { return order[values[i]] < order[values[j]] })
	return values
}
