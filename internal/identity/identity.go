// Package identity works out which agent session a command belongs to, so a
// hold, note or undo from inside an agent lands on that agent's session.
//
// Order: RECALLSTACK_SESSION_ID (Claude Code, set at session start through
// CLAUDE_ENV_FILE), CODEX_THREAD_ID (Codex exports its thread id to the
// commands it runs), the process tree (a pid link written at session start),
// the newest session of the detected agent in this folder within 12 hours,
// else the person typing in a terminal ("hand").
package identity

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/state"
)

type Identity struct {
	Agent     string // claude | codex | cursor | copilot | antigravity | hand
	SessionID string // empty when by hand
	How       string // env | codex_env | process_tree | newest_session | hand
	Session   state.Session
}

func (i Identity) ByHand() bool { return i.Agent == "hand" || i.SessionID == "" }

var knownAgents = map[string]bool{"claude": true, "codex": true, "cursor": true, "copilot": true, "antigravity": true}

// Find resolves the identity for a command running in cwd.
func Find(cwd string) Identity {
	if id := strings.TrimSpace(os.Getenv("RECALLSTACK_SESSION_ID")); id != "" {
		agent := strings.TrimSpace(os.Getenv("RECALLSTACK_AGENT"))
		if !knownAgents[agent] {
			agent = "claude"
		}
		session, _ := state.LoadSession(agent, id)
		return Identity{Agent: agent, SessionID: id, How: "env", Session: session}
	}
	if id := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")); id != "" {
		session, _ := state.LoadSession("codex", id)
		return Identity{Agent: "codex", SessionID: id, How: "codex_env", Session: session}
	}
	detected := ""
	for _, process := range Ancestors(os.Getpid(), 16) {
		if link, ok := state.LoadPIDLink(process.PID); ok && linkStillValid(link, process) {
			session, _ := state.LoadSession(link.Agent, link.SessionID)
			return Identity{Agent: link.Agent, SessionID: link.SessionID, How: "process_tree", Session: session}
		}
		if detected == "" {
			detected = agentFromProcess(process)
		}
	}
	if detected != "" {
		if session, ok := state.NewestSession(detected, cwd, 12*time.Hour); ok {
			return Identity{Agent: detected, SessionID: session.SessionID, How: "newest_session", Session: session}
		}
	}
	return Identity{Agent: "hand", How: "hand"}
}

// Process is one ancestor.
type Process struct {
	PID     int
	PPID    int
	Comm    string
	Exe     string
	Started string // Linux start time in clock ticks, for pid reuse checks
}

// Ancestors returns the parents of pid, nearest first.
func Ancestors(pid int, limit int) []Process {
	out := []Process{}
	current, ok := Lookup(pid)
	if !ok {
		return out
	}
	for len(out) < limit && current.PPID > 1 {
		parent, ok := Lookup(current.PPID)
		if !ok {
			break
		}
		out = append(out, parent)
		current = parent
	}
	return out
}

// Lookup reads one process.
func Lookup(pid int) (Process, bool) {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return Process{}, false
		}
		text := string(raw)
		open := strings.Index(text, "(")
		close := strings.LastIndex(text, ")")
		if open < 0 || close < open {
			return Process{}, false
		}
		fields := strings.Fields(text[close+1:])
		if len(fields) < 20 {
			return Process{}, false
		}
		ppid, _ := strconv.Atoi(fields[1])
		exe, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
		return Process{PID: pid, PPID: ppid, Comm: text[open+1 : close], Exe: exe, Started: fields[19]}, true
	case "darwin", "freebsd":
		output, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return Process{}, false
		}
		fields := strings.Fields(strings.TrimSpace(string(output)))
		if len(fields) < 2 {
			return Process{}, false
		}
		ppid, _ := strconv.Atoi(fields[0])
		comm := strings.Join(fields[1:], " ")
		return Process{PID: pid, PPID: ppid, Comm: filepath.Base(comm), Exe: comm}, true
	}
	return Process{}, false
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true, "tcsh": true, "csh": true, "env": true, "timeout": true, "recallstack": true, "recallstack.exe": true, "cmd.exe": true, "powershell.exe": true, "pwsh": true, "pwsh.exe": true, "sudo": true, "nohup": true}

// IsShell reports wrapper processes that sit between an agent and a command.
func IsShell(process Process) bool {
	return shells[strings.ToLower(process.Comm)] || shells[strings.ToLower(filepath.Base(process.Exe))]
}

// AgentProcess is the nearest non-shell ancestor of the current process: the
// agent that ran this hook.
func AgentProcess() (Process, bool) {
	for _, process := range Ancestors(os.Getpid(), 8) {
		if !IsShell(process) {
			return process, true
		}
	}
	return Process{}, false
}

func agentFromProcess(process Process) string {
	names := strings.ToLower(process.Comm + " " + process.Exe)
	switch {
	case strings.Contains(names, "codex"):
		return "codex"
	case strings.Contains(names, "claude"):
		return "claude"
	case strings.Contains(names, "cursor"):
		return "cursor"
	case strings.Contains(names, "copilot"):
		return "copilot"
	case strings.Contains(names, "antigravity"):
		return "antigravity"
	}
	return ""
}

func linkStillValid(link state.PIDLink, process Process) bool {
	if link.Started == "" || process.Started == "" {
		return true
	}
	return link.Started == process.Started
}

// WriteLink records that the agent process runs sessionID.
func WriteLink(agent, sessionID, cwd string) (int, error) {
	process, ok := AgentProcess()
	if !ok {
		return 0, nil
	}
	return process.PID, state.WriteJSON(state.PIDPath(process.PID), state.PIDLink{
		PID: process.PID, Agent: agent, SessionID: sessionID, CWD: cwd, Started: process.Started,
		WrittenAt: time.Now().UTC().Format(time.RFC3339),
	})
}
