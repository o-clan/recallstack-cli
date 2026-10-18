package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/secrets"
	"github.com/o-clan/recallstack-cli/internal/state"
)

// Candidate is one record the headless pass found (contract section 12).
type Candidate struct {
	Kind        string   `json:"kind"`
	What        string   `json:"what"`
	Why         *string  `json:"why"`
	Next        *string  `json:"next"`
	InsteadOf   []string `json:"instead_of"`
	Where       Where    `json:"where"`
	Signal      string   `json:"signal"`
	SaidBy      string   `json:"said_by"`
	Quote       *string  `json:"quote"`
	ReplacesRef *string  `json:"replaces_ref"`
	AnswersRef  *string  `json:"answers_ref"`
	AnswersText *string  `json:"answers_text"`
}

type Where struct {
	Scope string  `json:"scope"`
	Path  *string `json:"path"`
}

type ActedAgainst struct {
	Ref          string `json:"ref"`
	WhatHappened string `json:"what_happened"`
}

type Extraction struct {
	Candidates   []Candidate   `json:"candidates"`
	ActedAgainst *ActedAgainst `json:"acted_against"`
}

// extractionSchema is the contract's schema in the strict form structured
// output needs (every property required, nullable via type unions). Length
// limits are enforced after parsing.
const extractionSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["candidates", "acted_against"],
  "properties": {
    "candidates": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["kind", "what", "why", "next", "instead_of", "where", "signal", "said_by", "quote", "replaces_ref", "answers_ref", "answers_text"],
        "properties": {
          "kind": { "type": "string", "enum": ["goal", "focus", "decision", "question", "taste"] },
          "what": { "type": "string" },
          "why": { "type": ["string", "null"] },
          "next": { "type": ["string", "null"] },
          "instead_of": { "type": "array", "items": { "type": "string" } },
          "where": {
            "type": "object",
            "additionalProperties": false,
            "required": ["scope", "path"],
            "properties": {
              "scope": { "type": "string", "enum": ["everywhere", "project", "path"] },
              "path": { "type": ["string", "null"] }
            }
          },
          "signal": { "type": "string", "enum": ["correction", "statement", "choice", "answer", "accepted_proposal"] },
          "said_by": { "type": "string", "enum": ["person", "agent"] },
          "quote": { "type": ["string", "null"] },
          "replaces_ref": { "type": ["string", "null"] },
          "answers_ref": { "type": ["string", "null"] },
          "answers_text": { "type": ["string", "null"] }
        }
      }
    },
    "acted_against": {
      "anyOf": [
        { "type": "null" },
        {
          "type": "object",
          "additionalProperties": false,
          "required": ["ref", "what_happened"],
          "properties": { "ref": { "type": "string" }, "what_happened": { "type": "string" } }
        }
      ]
    }
  }
}`

var agentNames = map[string]string{"claude": "Claude Code", "codex": "Codex", "cursor": "Cursor", "copilot": "Copilot", "antigravity": "Antigravity"}

func agentName(agent string) string {
	if name, ok := agentNames[agent]; ok {
		return name
	}
	return agent
}

func extractionPrompt(agent, person, brief string, value turn) string {
	users := []string{}
	for _, message := range value.UserMessages {
		users = append(users, truncate(message, 6000))
	}
	assistants := value.AssistantMessages
	if len(assistants) > 3 {
		assistants = assistants[len(assistants)-3:]
	}
	trimmed := []string{}
	for _, message := range assistants {
		trimmed = append(trimmed, truncate(message, 4000))
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"user_messages":            users,
		"assistant_messages":       trimmed,
		"tools_used":               uniqueStrings(value.ToolNames),
		"recallstack_commands_run": uniqueStrings(value.RecallstackCommands),
	}, "", "  ")
	if strings.TrimSpace(brief) == "" {
		brief = "(no brief yet)"
	}
	return fmt.Sprintf(`You read one finished turn between %[1]s and their coding agent (%[2]s) and pick out what %[1]s decided, wants or prefers, so every agent can follow it later.

Record only these kinds:
- goal: what the work is for, or what comes first.
- focus: the piece of work under way across sessions (a feature, a phase, a screen); put the next step in "next". An instruction for this turn only ("don't change files yet", "just look first", "keep it short this time") is not a focus or any other kind.
- decision: a choice about how things should be, with its reason in "why" and the options that lost in "instead_of".
- question: something %[1]s left open on purpose.
- taste: how %[1]s likes things done (writing, interfaces, code, replies, process).

The test for every candidate: would it still be true if the code were rewritten from scratch? If it only describes how today's code works, what the agent did, or a fact about files, tools or commands, leave it out.

Rules:
- "said_by": "person" only when %[1]s said or chose it in this turn. Then "quote" is their exact words, copied character for character from one of the user messages below (the shortest part that carries it, at most 500 characters). Anything only the agent proposed, which %[1]s didn't clearly accept, is "said_by": "agent" with "quote": null.
- "signal": "correction" (they corrected the agent), "statement" (they stated it), "choice" (they picked between options; the others go in "instead_of"), "answer" (they answered a question; set "answers_ref" or "answers_text"), "accepted_proposal" (they accepted what the agent proposed).
- "what" is one plain sentence in %[1]s's terms, at most 300 characters. "why" is at most 300 characters, or null.
- Use the refs from the brief (D1, T3, Q2 and so on) for "replaces_ref" only when %[1]s says in so many words that an existing thought no longer holds ("actually", "forget that", "from now on", "we're dropping"). When they just say something that disagrees with a thought in the brief, leave "replaces_ref" null: Recallstack asks them which one holds. Use "answers_ref" when they answer an open question there.
- "where": "everywhere" for taste that isn't about this project, "project" otherwise, "path" with a path relative to the repo root when it applies only to one part.
- "acted_against": when the agent went against a decision in force in the brief without stopping to ask, give {"ref", "what_happened"}; otherwise null.
- A request for work ("add a script that deploys from my branch", "make the button blue", "deploy phase 1") is the task, not a decision. It's a decision only when they say how things should be from now on.
- "recallstack_commands_run" is what the agent already recorded this turn (notes, holds and their answers, adjustments, drops). Leave those thoughts out: an answer to a hold ("go ahead anyway", "change the plan") is already recorded as an exception or kept decision, not a new decision.
- Pleasantries, status chatter, and thoughts already in the brief with the same meaning are not candidates. Most turns have none: then return an empty list.

The brief this session started with (for refs):
<brief>
%[3]s
</brief>

The turn. User messages are %[1]s's own words:
<turn>
%[4]s
</turn>

Return only JSON that matches the schema.`, person, agentName(agent), strings.TrimSpace(brief), string(payload))
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// extractorsFor lists the headless agents that can read the turn, in the
// order to try them: the same agent when it can run headless, then the others
// installed. A runner that fails (signed out, rate limited) falls through to
// the next.
func extractorsFor(agent string) []string {
	order := map[string][]string{
		"claude":      {"claude", "codex", "copilot"},
		"codex":       {"codex", "claude", "copilot"},
		"antigravity": {"codex", "claude", "copilot"},
		"copilot":     {"copilot", "claude", "codex"},
		"cursor":      {"claude", "codex", "copilot"},
	}[agent]
	out := []string{}
	for _, candidate := range order {
		if _, err := exec.LookPath(binaryFor(candidate)); err == nil {
			out = append(out, candidate)
		}
	}
	return out
}

func binaryFor(agent string) string {
	switch agent {
	case "claude":
		return envOr("CLAUDE_BIN", "claude")
	case "codex":
		return envOr("CODEX_BIN", "codex")
	case "copilot":
		return envOr("COPILOT_BIN", "copilot")
	}
	return agent
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// WorkerModel is the model the headless pass uses for agent, if one is set.
func WorkerModel(agent string) string {
	for _, name := range []string{"RECALLSTACK_" + strings.ToUpper(agent) + "_WORKER_MODEL", "RECALLSTACK_WORKER_MODEL"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	var settings map[string]map[string]any
	state.ReadJSON(state.AgentSettingsPath(), &settings)
	if value, ok := settings[agent]["worker_model"].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// RunHeadless runs prompt through the given agent and returns its JSON answer.
// It's shared by end-of-turn capture and import.
func RunHeadless(ctx context.Context, runner, prompt, schema string) ([]byte, error) {
	env := append(os.Environ(), "RECALLSTACK_HOOK_ACTIVE=1")
	temporary, err := os.MkdirTemp("", "recallstack-extract-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	model := WorkerModel(runner)
	switch runner {
	case "claude":
		args := []string{"-p", "--output-format", "json", "--json-schema", schema, "--tools", "", "--no-session-persistence",
			"--append-system-prompt", "You are a strict JSON extraction pass for Recallstack. Reply with JSON only."}
		if model != "" {
			args = append(args, "--model", model)
		}
		cmd := exec.CommandContext(ctx, binaryFor("claude"), args...)
		cmd.Dir = temporary
		cmd.Env = env
		cmd.Stdin = strings.NewReader(prompt)
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("claude -p failed: %w", err)
		}
		return unwrapClaude(output)
	case "codex":
		schemaPath := filepath.Join(temporary, "schema.json")
		outputPath := filepath.Join(temporary, "output.json")
		if err := os.WriteFile(schemaPath, []byte(schema), 0o600); err != nil {
			return nil, err
		}
		args := []string{"exec", "--ephemeral", "--disable", "hooks", "--skip-git-repo-check", "--sandbox", "read-only", "-C", temporary, "--output-schema", schemaPath, "--output-last-message", outputPath}
		if model != "" {
			args = append(args, "--model", model)
		}
		args = append(args, "-")
		cmd := exec.CommandContext(ctx, binaryFor("codex"), args...)
		cmd.Dir = temporary
		cmd.Env = env
		cmd.Stdin = strings.NewReader(prompt)
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("codex exec failed: %w: %s", err, truncate(string(output), 400))
		}
		return os.ReadFile(outputPath)
	case "copilot":
		args := []string{"--config-dir", temporary, "--no-custom-instructions", "--output-format", "text", "--silent", "-p", prompt + "\n\nThe JSON schema:\n" + schema}
		if model != "" {
			args = append([]string{"--model", model}, args...)
		}
		cmd := exec.CommandContext(ctx, binaryFor("copilot"), args...)
		cmd.Dir = temporary
		cmd.Env = env
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("copilot -p failed: %w", err)
		}
		return extractJSONObject(output), nil
	}
	return nil, fmt.Errorf("no headless agent available")
}

func unwrapClaude(output []byte) ([]byte, error) {
	var outer map[string]any
	if json.Unmarshal(output, &outer) != nil {
		return extractJSONObject(output), nil
	}
	if _, ok := outer["candidates"]; ok {
		return output, nil
	}
	if structured, ok := outer["structured_output"]; ok && structured != nil {
		return json.Marshal(structured)
	}
	if result, ok := outer["result"].(string); ok && strings.TrimSpace(result) != "" {
		return extractJSONObject([]byte(result)), nil
	}
	return nil, fmt.Errorf("claude returned no JSON result")
}

// extractJSONObject finds the outermost {...} in text (models sometimes add fences).
func extractJSONObject(raw []byte) []byte {
	text := strings.TrimSpace(string(raw))
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return []byte(text)
	}
	return []byte(text[start : end+1])
}

var validKinds = map[string]bool{"goal": true, "focus": true, "decision": true, "question": true, "taste": true}
var validSignals = map[string]bool{"correction": true, "statement": true, "choice": true, "answer": true, "accepted_proposal": true}

// normalizeCandidates enforces the contract limits, checks that a person's
// quote really is in the person's messages, and strips secrets from every
// string. It returns the number of secrets replaced.
func normalizeCandidates(values []Candidate, userMessages []string) ([]Candidate, int) {
	stripped := 0
	strip := func(value string) string {
		out, count := secrets.Strip(value)
		stripped += count
		return out
	}
	stripPtr := func(value *string, limit int) *string {
		if value == nil {
			return nil
		}
		text := strings.TrimSpace(*value)
		if text == "" {
			return nil
		}
		text = truncate(strip(text), limit)
		return &text
	}
	haystack := normalizeQuote(strings.Join(userMessages, "\n"))
	out := []Candidate{}
	for _, value := range values {
		value.Kind = strings.ToLower(strings.TrimSpace(value.Kind))
		if !validKinds[value.Kind] {
			continue
		}
		value.What = truncate(strip(compact(value.What)), 300)
		if value.What == "" || pleasantryPattern.MatchString(value.What) {
			continue
		}
		value.Why = stripPtr(value.Why, 300)
		value.Next = stripPtr(value.Next, 200)
		value.ReplacesRef = stripPtr(value.ReplacesRef, 80)
		value.AnswersRef = stripPtr(value.AnswersRef, 80)
		value.AnswersText = stripPtr(value.AnswersText, 300)
		insteadOf := []string{}
		for _, option := range value.InsteadOf {
			if option = truncate(strip(compact(option)), 120); option != "" && len(insteadOf) < 5 {
				insteadOf = append(insteadOf, option)
			}
		}
		value.InsteadOf = insteadOf
		if !validSignals[value.Signal] {
			value.Signal = "statement"
		}
		switch value.Where.Scope {
		case "everywhere", "project":
			value.Where.Path = nil
		case "path":
			value.Where.Path = stripPtr(value.Where.Path, 300)
			if value.Where.Path == nil {
				value.Where.Scope = "project"
			}
		default:
			value.Where = Where{Scope: "project"}
		}
		if value.Kind == "focus" && value.Where.Scope == "everywhere" {
			value.Where.Scope = "project"
		}
		// A quote attributed to the person must be in the person's words.
		if value.Quote != nil {
			raw := strings.TrimSpace(*value.Quote)
			if raw == "" {
				value.Quote = nil
			} else {
				verbatim := strings.Contains(haystack, normalizeQuote(raw))
				quote := truncate(strip(raw), 500)
				value.Quote = &quote
				if !verbatim {
					value.Quote = nil
				}
			}
		}
		if value.SaidBy != "person" || value.Quote == nil {
			value.SaidBy = "agent"
		}
		out = append(out, value)
		if len(out) == 12 {
			break
		}
	}
	return out, stripped
}

func normalizeQuote(value string) string {
	value = strings.ToLower(value)
	replacer := strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`, "…", "...")
	return strings.Join(strings.Fields(replacer.Replace(value)), " ")
}

// extract runs the headless pass over one turn.
func extract(ctx context.Context, agent, person, brief string, value turn) (Extraction, string, error) {
	runners := extractorsFor(agent)
	if len(runners) == 0 {
		return Extraction{}, "", fmt.Errorf("no headless agent (claude, codex or copilot) on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	prompt := extractionPrompt(agent, person, brief, value)
	failures := []string{}
	for _, runner := range runners {
		raw, err := RunHeadless(ctx, runner, prompt, extractionSchema)
		if err == nil {
			var result Extraction
			if err = json.Unmarshal(raw, &result); err == nil {
				return result, runner, nil
			}
			err = fmt.Errorf("unreadable extraction: %w", err)
		}
		failures = append(failures, runner+": "+err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	return Extraction{}, strings.Join(runners, ","), errors.New(strings.Join(failures, "; "))
}
