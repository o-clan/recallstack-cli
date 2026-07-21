package integrations

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Target identifies an agent integration managed by Recallstack.
type Target string

const (
	TargetCodex       Target = "codex"
	TargetClaude      Target = "claude"
	TargetCursor      Target = "cursor"
	TargetCopilot     Target = "copilot"
	TargetAntigravity Target = "antigravity"
	TargetAll         Target = "all"
)

// Scope selects repository-local or user-global agent configuration.
type Scope string

const (
	ScopeLocal  Scope = "local"
	ScopeGlobal Scope = "global"
)

type InstallOptions struct {
	Scope       Scope
	WorkerModel string
}

type UninstallOptions struct {
	Scope             Scope
	PurgeProjectState bool
}

type CommandResult struct {
	Status int
	Stdout string
	Stderr string
}

type CommandRunner func(name string, args ...string) (CommandResult, error)

// Manager owns all filesystem and process interactions for agent integrations.
// CWD and Home are fields so callers and tests can isolate local and global installs.
type Manager struct {
	CWD        string
	Home       string
	Executable string
	Version    string
	Run        CommandRunner
}

func New(cwd string) (*Manager, error) {
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	return &Manager{CWD: cwd, Home: home, Executable: executable, Version: "dev", Run: runCommand}, nil
}

func runCommand(name string, args ...string) (CommandResult, error) {
	command := exec.Command(name, args...)
	stdout := new(strings.Builder)
	stderr := new(strings.Builder)
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if command.ProcessState != nil {
		result.Status = command.ProcessState.ExitCode()
	} else {
		result.Status = -1
	}
	return result, err
}

func ParseTarget(value string) (Target, error) {
	target := Target(strings.ToLower(strings.TrimSpace(value)))
	if !target.Valid() {
		return "", fmt.Errorf("unsupported target %q; use codex, claude, cursor, copilot, antigravity, or all", value)
	}
	return target, nil
}

func (target Target) Valid() bool {
	switch target {
	case TargetCodex, TargetClaude, TargetCursor, TargetCopilot, TargetAntigravity, TargetAll:
		return true
	default:
		return false
	}
}

func ParseScope(value string) (Scope, error) {
	scope := Scope(strings.ToLower(strings.TrimSpace(value)))
	if scope == "" {
		return ScopeLocal, nil
	}
	if scope != ScopeLocal && scope != ScopeGlobal {
		return "", fmt.Errorf("invalid scope %q; use local or global", value)
	}
	return scope, nil
}

func (m *Manager) normalize(scope Scope) error {
	if m == nil {
		return errors.New("integration manager is nil")
	}
	if strings.TrimSpace(m.CWD) == "" {
		return errors.New("integration manager CWD is required")
	}
	if strings.TrimSpace(m.Home) == "" {
		return errors.New("integration manager Home is required")
	}
	if scope != ScopeLocal && scope != ScopeGlobal {
		return fmt.Errorf("invalid integration scope %q", scope)
	}
	if m.Run == nil {
		m.Run = runCommand
	}
	if m.Version == "" {
		m.Version = "dev"
	}
	if strings.TrimSpace(m.Executable) == "" {
		m.Executable = "recallstack"
	}
	m.CWD = filepath.Clean(m.CWD)
	m.Home = filepath.Clean(m.Home)
	return nil
}

func targets(target Target) []Target {
	if target != TargetAll {
		return []Target{target}
	}
	return []Target{TargetCodex, TargetClaude, TargetCursor, TargetCopilot, TargetAntigravity}
}

func modelTargets(target Target) []Target {
	if target == TargetAll {
		return []Target{TargetCodex, TargetClaude, TargetCopilot}
	}
	if target == TargetCodex || target == TargetClaude || target == TargetCopilot {
		return []Target{target}
	}
	return nil
}
