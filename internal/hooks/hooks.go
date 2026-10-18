// Package hooks runs inside the agents. Every handler fails open: whatever
// goes wrong, the agent carries on, and only the before-tool hook can ever
// stop a tool, and only while a hold is open.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/secrets"
	"github.com/o-clan/recallstack-cli/internal/state"
)

const (
	hiddenWorkerCommand  = "__hook-worker"
	hiddenRetryCommand   = "__hook-retry"
	hiddenRefreshCommand = "__hook-refresh"
)

func HiddenWorkerCommand() string  { return hiddenWorkerCommand }
func HiddenRetryCommand() string   { return hiddenRetryCommand }
func HiddenRefreshCommand() string { return hiddenRefreshCommand }

type Options struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
}

// Canonical moments, whatever each agent calls them.
const (
	momentStart          = "start"
	momentPrompt         = "prompt"
	momentPreTool        = "pretool"
	momentStop           = "stop"
	momentEnd            = "end"
	momentCursorPrompt   = "cursor_prompt"
	momentCursorResponse = "cursor_response"
)

type hookRun struct {
	agent   string
	event   string
	moment  string
	payload map[string]any
	out     io.Writer
	version string
	started time.Time
	cwd     string
	// vscodeShape is true for Copilot events in the Claude-compatible form.
	vscodeShape bool
}

func NormalizeAgent(value string) string {
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
	}
	return ""
}

func momentFor(agent, event string) (string, bool) {
	key := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(event))
	switch agent {
	case "cursor":
		switch key {
		case "beforesubmitprompt":
			return momentCursorPrompt, false
		case "afteragentresponse":
			return momentCursorResponse, false
		case "beforeshellexecution":
			return momentPreTool, false
		case "stop":
			return momentStop, false
		}
		return "", false
	case "antigravity":
		return momentStop, false
	case "copilot":
		vscode := event != "" && event[0] >= 'A' && event[0] <= 'Z'
		switch key {
		case "sessionstart":
			return momentStart, vscode
		case "userpromptsubmitted", "userpromptsubmit":
			return momentPrompt, vscode
		case "pretooluse":
			return momentPreTool, vscode
		case "sessionend":
			return momentEnd, vscode
		case "stop", "agentstop":
			return momentStop, vscode
		}
		return "", false
	}
	switch key {
	case "sessionstart":
		return momentStart, false
	case "userpromptsubmit":
		return momentPrompt, false
	case "pretooluse":
		return momentPreTool, false
	case "stop":
		return momentStop, false
	case "sessionend":
		return momentEnd, false
	}
	return "", false
}

// Run handles one hook call. It never returns an error to the agent for
// anything but an unknown agent name.
func Run(ctx context.Context, agentArg, event string, options Options) (err error) {
	started := time.Now()
	agent := NormalizeAgent(agentArg)
	if agent == "" {
		return fmt.Errorf("unsupported hook agent %q", agentArg)
	}
	out := options.Stdout
	if out == nil {
		out = os.Stdout
	}
	if os.Getenv("RECALLSTACK_HOOK_ACTIVE") == "1" {
		// A headless extraction pass started by Recallstack: stay out of it.
		if agent == "antigravity" {
			fmt.Fprintln(out, `{"decision":"allow"}`)
		}
		return nil
	}
	stdin := options.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	payload := readPayload(stdin)
	moment, vscode := momentFor(agent, event)
	run := &hookRun{agent: agent, event: event, moment: moment, payload: payload, out: out, version: options.Version, started: started, vscodeShape: vscode}
	run.cwd = run.payloadCWD()
	outcome := "ok"
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = fmt.Sprint("panic: ", recovered)
			err = nil
		}
		if agent == "antigravity" {
			fmt.Fprintln(out, `{"decision":"allow"}`)
		}
		state.AppendLog("hooks.log", map[string]any{"agent": agent, "event": event, "ms": float64(time.Since(started).Microseconds()) / 1000, "outcome": outcome})
	}()
	if os.Getenv("RECALLSTACK_HOOK_DEBUG") == "1" {
		state.AppendLog("hook-payloads.log", map[string]any{"agent": agent, "event": event, "payload": stripAll(payload)})
	}
	switch moment {
	case momentStart:
		outcome = run.sessionStart(ctx)
	case momentPrompt:
		outcome = run.promptSubmit(ctx)
	case momentPreTool:
		outcome = run.preTool()
	case momentStop:
		outcome = run.stop()
	case momentEnd:
		outcome = run.sessionEnd(ctx)
	case momentCursorPrompt, momentCursorResponse:
		outcome = run.cursorPair()
	default:
		outcome = "ignored"
	}
	return nil
}

func readPayload(reader io.Reader) map[string]any {
	data, err := io.ReadAll(io.LimitReader(reader, 16<<20))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || value == nil {
		return map[string]any{}
	}
	return value
}

func (h *hookRun) payloadCWD() string {
	if roots, ok := h.payload["workspace_roots"].([]any); ok {
		for _, root := range roots {
			if value := stringValue(root); value != "" {
				return value
			}
		}
	}
	if paths, ok := h.payload["workspacePaths"].([]any); ok {
		for _, path := range paths {
			if value := stringValue(path); value != "" {
				return value
			}
		}
	}
	if value := stringValue(h.payload["cwd"]); value != "" {
		return value
	}
	cwd, _ := os.Getwd()
	return cwd
}

func (h *hookRun) sessionID() string {
	switch h.agent {
	case "cursor":
		return stringValue(h.payload["conversation_id"])
	case "antigravity":
		return firstNonEmpty(stringValue(h.payload["conversationId"]), sha1Hex(stringValue(h.payload["transcriptPath"])))
	case "copilot":
		for _, key := range []string{"sessionId", "session_id", "conversationId", "conversation_id"} {
			if value := stringValue(h.payload[key]); value != "" {
				return value
			}
		}
		return "cwd-" + sha1Hex(h.cwd)[:16]
	}
	return stringValue(h.payload["session_id"])
}

func (h *hookRun) writeJSON(value any) {
	encoder := json.NewEncoder(h.out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

// contextOutput prints additional context in the agent's shape.
func (h *hookRun) contextOutput(eventName, context, systemMessage string) {
	if h.agent == "copilot" && !h.vscodeShape {
		// Copilot CLI ignores start and prompt output; its brief lives in the
		// instructions file instead.
		return
	}
	output := map[string]any{}
	if context != "" {
		output["hookSpecificOutput"] = map[string]any{"hookEventName": eventName, "additionalContext": context}
	}
	if systemMessage != "" {
		output["systemMessage"] = systemMessage
	}
	if len(output) > 0 {
		h.writeJSON(output)
	}
}

// stripAll strips secrets from every string in a decoded JSON value.
func stripAll(value any) any {
	switch typed := value.(type) {
	case string:
		return secrets.StripString(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = stripAll(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = stripAll(item)
		}
		return out
	}
	return value
}
