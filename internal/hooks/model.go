package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type turn struct {
	DedupeKey          string
	TurnID             string
	LastAssistant      string
	UserMessages       []string
	AssistantMessages  []string
	Items              []any
	ToolNames          []string
	TranscriptPath     string
	UserStepIndex      int
	AssistantStepIndex int
}

type extractedTurn struct {
	TurnKind      string   `json:"turn_kind"`
	UserIntent    string   `json:"user_intent"`
	AgentIntent   string   `json:"agent_intent"`
	KeyActions    []string `json:"key_actions"`
	Tradeoffs     []string `json:"tradeoffs"`
	Outcome       string   `json:"outcome"`
	OpenQuestions []string `json:"open_questions"`
}

var pleasantryPattern = regexp.MustCompile(`(?i)^(thanks|thank you|thx|got it(?:,? thanks)?|okay,? thanks|ok,? thanks|cool,? thanks|nice,? thanks|appreciate it|sounds good,? thanks|perfect,? thanks)[!. ]*$`)

func last(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return strings.TrimSpace(items[len(items)-1])
}

func fallbackExtracted(value turn) extractedTurn {
	userIntent := compact(last(value.UserMessages))
	outcome := compact(last(value.AssistantMessages))
	if outcome == "" {
		outcome = compact(value.LastAssistant)
	}
	kind := "thin"
	if pleasantryPattern.MatchString(userIntent) {
		kind = "pleasantry"
	}
	agentIntent := ""
	if outcome != "" {
		agentIntent = "Respond to the user's latest request and move the work forward."
	}
	return extractedTurn{TurnKind: kind, UserIntent: userIntent, AgentIntent: agentIntent, Outcome: outcome}
}

func normalizeExtracted(value extractedTurn, source turn) extractedTurn {
	fallback := fallbackExtracted(source)
	switch value.TurnKind {
	case "substantial", "thin", "pleasantry":
	default:
		value.TurnKind = fallback.TurnKind
	}
	if fallback.TurnKind == "pleasantry" {
		value.TurnKind = "pleasantry"
	} else if value.TurnKind == "pleasantry" {
		value.TurnKind = "thin"
	}
	value.UserIntent = compact(value.UserIntent)
	if value.UserIntent == "" || regexp.MustCompile(`(?i)(capture a recallstack memory|provided .* turn payload|extraction task|hook task|json task|recallstack ingestion task)`).MatchString(value.UserIntent) {
		value.UserIntent = fallback.UserIntent
	}
	value.AgentIntent = compact(value.AgentIntent)
	value.Outcome = compact(value.Outcome)
	if value.Outcome == "" {
		value.Outcome = fallback.Outcome
	}
	value.KeyActions = cleanList(value.KeyActions)
	value.Tradeoffs = cleanList(value.Tradeoffs)
	value.OpenQuestions = cleanList(value.OpenQuestions)
	return value
}

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func formatSummary(value extractedTurn) string {
	lines := make([]string, 0, 6)
	if value.UserIntent != "" {
		lines = append(lines, "User intent: "+value.UserIntent)
	}
	if value.TurnKind == "substantial" && value.AgentIntent != "" {
		lines = append(lines, "Agent intent: "+value.AgentIntent)
	}
	if value.TurnKind == "substantial" && len(value.KeyActions) > 0 {
		lines = append(lines, "Actions: "+strings.Join(value.KeyActions, "; "))
	}
	if value.TurnKind == "substantial" && len(value.Tradeoffs) > 0 {
		lines = append(lines, "Tradeoffs: "+strings.Join(value.Tradeoffs, "; "))
	}
	if value.Outcome != "" {
		lines = append(lines, "Outcome: "+value.Outcome)
	}
	if value.TurnKind == "substantial" && len(value.OpenQuestions) > 0 {
		lines = append(lines, "Open questions: "+strings.Join(value.OpenQuestions, "; "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func turnSnapshot(value extractedTurn, summary string) map[string]any {
	return map[string]any{
		"turn_kind":    value.TurnKind,
		"user_intent":  compact(value.UserIntent),
		"agent_intent": compact(value.AgentIntent),
		"outcome":      compact(value.Outcome),
		"summary":      compact(summary),
	}
}

func extractorPrompt(agent string, value turn, previous any) string {
	payload := map[string]any{
		"turn_id":                value.TurnID,
		"dedupe_key":             value.DedupeKey,
		"last_assistant_message": value.LastAssistant,
		"user_messages":          value.UserMessages,
		"assistant_messages":     value.AssistantMessages,
		"items":                  value.Items,
		"tool_names":             value.ToolNames,
		"previous_turn_snapshot": previous,
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return fmt.Sprintf(`You are extracting Recallstack turn memory from a completed %s turn.
Return JSON with exactly these fields: turn_kind, user_intent, agent_intent, key_actions, tradeoffs, outcome, open_questions.
Use only the provided turn payload. The latest real user message is the source of truth for user_intent. Use previous_turn_snapshot only to clarify a referential latest request. Capture project-relevant actions, decisions, tradeoffs, outcome, and open questions. Avoid terminal noise and memory-hook housekeeping. turn_kind must be substantial, thin, or pleasantry.

TURN PAYLOAD
%s`, agent, data)
}

func workerModel(cwd, agent string) string {
	names := []string{"RECALLSTACK_" + strings.ToUpper(agent) + "_WORKER_MODEL"}
	if agent == "antigravity" {
		names = append(names, "RECALLSTACK_CODEX_WORKER_MODEL")
	}
	names = append(names, "RECALLSTACK_WORKER_MODEL")
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	settingsTarget := agent
	if agent == "antigravity" {
		settingsTarget = "codex"
	}
	for _, path := range []string{
		filepath.Join(cwd, ".recallstack", "agent-settings.json"),
		filepath.Join(userHome(), ".recallstack", "agent-settings.json"),
	} {
		settings := readJSONMap(path)
		if value := stringValue(mapValue(settings[settingsTarget])["worker_model"]); value != "" {
			return value
		}
	}
	return ""
}

func userHome() string {
	if value := strings.TrimSpace(os.Getenv("HOME")); value != "" {
		return value
	}
	value, _ := os.UserHomeDir()
	return value
}

func runCodexExtractor(ctx context.Context, rt *runtime, agentName, cwd, state string, value turn, previous any) extractedTurn {
	fallback := fallbackExtracted(value)
	temporary, err := os.MkdirTemp("", "recallstack-"+agentName+"-hook-")
	if err != nil {
		return fallback
	}
	defer os.RemoveAll(temporary)
	schemaPath := filepath.Join(temporary, "schema.json")
	outputPath := filepath.Join(temporary, "output.json")
	_ = os.WriteFile(schemaPath, []byte(extractSchemaJSON), 0o644)
	args := []string{"exec", "--ephemeral", "--disable", "codex_hooks", "--skip-git-repo-check", "-C", temporary, "--output-schema", schemaPath, "--output-last-message", outputPath, "-"}
	if model := workerModel(cwd, agentName); model != "" {
		args = append([]string{"exec", "--model", model}, args[1:]...)
	}
	displayName := agentName
	if agentName == "codex" {
		displayName = "Codex"
	} else if agentName == "antigravity" {
		displayName = "Google Antigravity"
	}
	result := rt.run(ctx, envCommand("CODEX_BIN", "codex"), args, CommandOptions{CWD: temporary, Env: rt.envWithHookActive(), Input: extractorPrompt(displayName, value, previous)})
	data, readErr := os.ReadFile(outputPath)
	if result.Status != 0 || readErr != nil {
		appendError(state, rt.nowUTC(), "Extractor failed.", map[string]any{"agent": agentName, "status": result.Status, "stdout": truncate(result.Stdout, 1200), "stderr": truncate(result.Stderr, 1200)})
		return fallback
	}
	var extracted extractedTurn
	if json.Unmarshal(data, &extracted) != nil {
		return fallback
	}
	return normalizeExtracted(extracted, value)
}

func runClaudeExtractor(ctx context.Context, rt *runtime, cwd, state string, value turn, previous any) extractedTurn {
	args := []string{"-p", "--output-format", "json", "--json-schema", extractSchemaJSON, "--tools", "", "--no-session-persistence", "--append-system-prompt", "You are a strict JSON extraction pass for Recallstack hook ingestion."}
	if model := workerModel(cwd, "claude"); model != "" {
		args = append([]string{"-p", "--model", model}, args[1:]...)
	}
	result := rt.run(ctx, envCommand("CLAUDE_BIN", "claude"), args, CommandOptions{CWD: cwd, Env: rt.envWithHookActive(), Input: extractorPrompt("Claude Code", value, previous)})
	if result.Status != 0 {
		return fallbackExtracted(value)
	}
	var outer map[string]any
	var extracted extractedTurn
	if json.Unmarshal([]byte(result.Stdout), &extracted) == nil && extracted.TurnKind != "" {
		return normalizeExtracted(extracted, value)
	}
	if json.Unmarshal([]byte(result.Stdout), &outer) == nil {
		if raw := stringValue(outer["result"]); raw != "" && json.Unmarshal([]byte(raw), &extracted) == nil {
			return normalizeExtracted(extracted, value)
		}
		if content, ok := outer["content"].([]any); ok {
			parts := make([]string, 0, len(content))
			for _, item := range content {
				entry := mapValue(item)
				if stringValue(entry["type"]) == "text" && stringValue(entry["text"]) != "" {
					parts = append(parts, stringValue(entry["text"]))
				}
			}
			if json.Unmarshal([]byte(strings.Join(parts, "\n")), &extracted) == nil {
				return normalizeExtracted(extracted, value)
			}
		}
	}
	appendError(state, rt.nowUTC(), "Claude extractor returned unreadable output.", map[string]any{"stdout": truncate(result.Stdout, 1200)})
	return fallbackExtracted(value)
}

func runCopilotExtractor(ctx context.Context, rt *runtime, cwd, state string, value turn, previous any) extractedTurn {
	temporary, err := os.MkdirTemp("", "recallstack-copilot-worker-")
	if err != nil {
		return normalizeCopilotExtracted(fallbackExtracted(value), value)
	}
	defer os.RemoveAll(temporary)
	args := []string{"--config-dir", temporary, "--no-custom-instructions", "--allow-all-tools", "--output-format", "text", "--silent", "-p", extractorPrompt("GitHub Copilot", value, previous)}
	if model := workerModel(cwd, "copilot"); model != "" {
		args = append([]string{"--model", model}, args...)
	}
	result := rt.run(ctx, envCommand("COPILOT_BIN", "copilot"), args, CommandOptions{CWD: temporary, Env: rt.envWithHookActive()})
	var extracted extractedTurn
	if result.Status == 0 && json.Unmarshal([]byte(result.Stdout), &extracted) == nil {
		return normalizeCopilotExtracted(extracted, value)
	}
	appendError(state, rt.nowUTC(), "Copilot extractor failed.", map[string]any{"status": result.Status, "stderr": truncate(result.Stderr, 1200)})
	return normalizeCopilotExtracted(fallbackExtracted(value), value)
}

func normalizeCopilotExtracted(value extractedTurn, source turn) extractedTurn {
	value = normalizeExtracted(value, source)
	value.UserIntent = compactWhitespace(value.UserIntent, 400)
	value.AgentIntent = compactWhitespace(value.AgentIntent, 400)
	value.Outcome = compactWhitespace(value.Outcome, 600)
	return value
}

const extractSchemaJSON = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,"properties":{"turn_kind":{"type":"string","enum":["substantial","thin","pleasantry"]},"user_intent":{"type":"string"},"agent_intent":{"type":"string"},"key_actions":{"type":"array","items":{"type":"string"}},"tradeoffs":{"type":"array","items":{"type":"string"}},"outcome":{"type":"string"},"open_questions":{"type":"array","items":{"type":"string"}}},"required":["turn_kind","user_intent","agent_intent","key_actions","tradeoffs","outcome","open_questions"]}`
