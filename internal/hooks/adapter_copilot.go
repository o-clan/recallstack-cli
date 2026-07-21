package hooks

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type copilotAdapter struct{}

func (copilotAdapter) run(_ context.Context, rt *runtime, event string, payload map[string]any) error {
	event = normalizeCopilotEvent(firstNonEmpty(event, stringValue(payload["hookEventName"]), stringValue(payload["hook_event_name"])))
	cwd := firstNonEmpty(stringValue(payload["cwd"]), rt.cwd)
	state := stateDir(cwd, "copilot")
	sessionKey := copilotSessionKey(payload, cwd)
	statePath := sessionPath(state, sessionKey)
	session := readJSONMap(statePath)
	switch event {
	case "sessionStart":
		err := updateState(statePath, rt.nowUTC(), map[string]any{"cwd": cwd, "repo_root": repoRoot(cwd), "last_session_start_at": rt.nowUTC().Format(timeFormat)})
		appendEvent(state, rt.nowUTC(), "session_started", map[string]any{"session_key": sessionKey})
		return err
	case "userPromptSubmitted":
		prompt := firstString(payload, "prompt", "message", "text")
		if strings.HasPrefix(prompt, "You are extracting Recallstack turn memory from a completed GitHub Copilot turn.") {
			return nil
		}
		if prompt != "" {
			err := updateState(statePath, rt.nowUTC(), map[string]any{"cwd": cwd, "repo_root": repoRoot(cwd), "last_prompt": prompt, "last_prompt_at": rt.nowUTC().Format(timeFormat)})
			appendEvent(state, rt.nowUTC(), "prompt_captured", map[string]any{"session_key": sessionKey})
			return err
		}
	case "sessionEnd", "agentStop":
		if strings.HasPrefix(stringValue(session["last_prompt"]), "You are extracting Recallstack turn memory from a completed GitHub Copilot turn.") {
			return nil
		}
		jobPath, err := enqueueJob(rt, "copilot", cwd, state, payload, map[string]any{"event_name": event, "session_key": sessionKey, "repo_root": repoRoot(cwd), "prompt": stringValue(session["last_prompt"]), "raw_payload": payload})
		_ = updateState(statePath, rt.nowUTC(), map[string]any{"cwd": cwd, "repo_root": repoRoot(cwd), "last_stop_event": event, "last_stop_at": rt.nowUTC().Format(timeFormat), "last_job_path": jobPath})
		return err
	}
	return nil
}

func (copilotAdapter) worker(ctx context.Context, rt *runtime, jobPath string) error {
	job := readJSONMap(jobPath)
	if len(job) == 0 {
		return nil
	}
	cwd := firstNonEmpty(stringValue(job["cwd"]), rt.cwd)
	state := stateDir(cwd, "copilot")
	target, ok := rt.resolveProject(ctx, cwd, state)
	if !ok {
		return nil
	}
	payload := mapValue(job["raw_payload"])
	if len(payload) == 0 {
		payload = mapValue(job["payload"])
	}
	repository := firstNonEmpty(stringValue(job["repo_root"]), repoRoot(cwd))
	prompt := firstNonEmpty(stringValue(job["prompt"]), stringValue(job["last_prompt"]))
	value, ok := findCopilotTurn(payload, repository, prompt)
	if !ok {
		appendError(state, rt.nowUTC(), "Copilot worker could not identify the latest completed turn.", map[string]any{"job_path": jobPath})
		return nil
	}
	sessionKey := stringValue(job["session_key"])
	statePath := sessionPath(state, sessionKey)
	session := readJSONMap(statePath)
	if stringValue(session["last_ingested_turn_id"]) == value.DedupeKey {
		removeJob(jobPath)
		return nil
	}
	extracted := runCopilotExtractor(ctx, rt, cwd, state, value, session["previous_turn_snapshot"])
	if extracted.TurnKind == "pleasantry" {
		removeJob(jobPath)
		return nil
	}
	summary := formatSummary(extracted)
	if summary == "" {
		removeJob(jobPath)
		return nil
	}
	_ = updateState(statePath, rt.nowUTC(), map[string]any{"previous_turn_snapshot": turnSnapshot(extracted, summary)})
	source := "copilot_vscode_hook"
	if stringValue(job["event_name"]) == "sessionEnd" {
		source = "copilot_cli_hook"
	}
	metadata := map[string]any{
		"agent": "github_copilot", "source": source, "extractor": "copilot_print",
		"repo_root": repository, "session_file": nullableText(value.TranscriptPath),
		"tool_names": value.ToolNames, "previous_turn_snapshot_used": session["previous_turn_snapshot"] != nil,
		"turn_kind": extracted.TurnKind, "user_intent": extracted.UserIntent, "agent_intent": extracted.AgentIntent,
		"key_actions": extracted.KeyActions, "tradeoffs": extracted.Tradeoffs, "open_questions": extracted.OpenQuestions,
	}
	idempotencyKey := "copilot-hook-" + sha1Hex(repository+":"+value.DedupeKey)
	result := rt.ingest(ctx, cwd, target, summary, idempotencyKey, metadata)
	if result.Status != 0 {
		if isRetryable(result) {
			return queueRetry(rt, "copilot", state, cwd, map[string]any{"cwd": cwd, "target": target, "summary": summary, "metadata": metadata, "idempotency_key": idempotencyKey, "session_key": sessionKey, "repo_root": repository, "dedupe_key": value.DedupeKey, "last_prompt": prompt}, result)
		}
		appendError(state, rt.nowUTC(), "Recallstack ingest failed from Copilot hook.", map[string]any{"status": result.Status, "stderr": truncate(result.Stderr, 1200)})
		return nil
	}
	markCopilotIngested(state, rt, sessionKey, repository, value.DedupeKey, prompt, cwd)
	appendEvent(state, rt.nowUTC(), "job_ingested", map[string]any{"session_key": sessionKey, "dedupe_key": value.DedupeKey, "target": target})
	removeJob(jobPath)
	return nil
}

func (copilotAdapter) retry(ctx context.Context, rt *runtime, path string) error {
	return processRetry(ctx, rt, "copilot", path, func(record map[string]any) {
		cwd := stringValue(record["cwd"])
		dedupe := stringValue(record["dedupe_key"])
		if dedupe == "" {
			dedupe = stringValue(mapValue(record["turn"])["dedupe_key"])
		}
		markCopilotIngested(stateDir(cwd, "copilot"), rt, stringValue(record["session_key"]), stringValue(record["repo_root"]), dedupe, stringValue(record["last_prompt"]), cwd)
	})
}

func normalizeCopilotEvent(value string) string {
	switch value {
	case "SessionStart":
		return "sessionStart"
	case "UserPromptSubmit":
		return "userPromptSubmitted"
	case "Stop":
		return "agentStop"
	}
	return value
}

func copilotSessionKey(payload map[string]any, cwd string) string {
	for _, key := range []string{"sessionId", "session_id", "conversationId", "conversation_id"} {
		if value := stringValue(payload[key]); value != "" {
			return value
		}
	}
	absolute, _ := filepath.Abs(cwd)
	return sha1Hex(absolute)
}

func repoRoot(cwd string) string {
	current, _ := filepath.Abs(cwd)
	for {
		if info, err := os.Stat(filepath.Join(current, ".git")); err == nil && info != nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cwd
		}
		current = parent
	}
}

func findCopilotTurn(payload map[string]any, repository, prompt string) (turn, bool) {
	for _, key := range []string{"transcript_path", "transcriptPath"} {
		if path := stringValue(payload[key]); path != "" {
			if value, ok := copilotTurnFromFile(path, prompt); ok {
				value.TranscriptPath = path
				return value, true
			}
		}
	}
	root := filepath.Join(userHome(), ".copilot", "session-state")
	files := []string{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool {
		left, _ := os.Stat(files[i])
		right, _ := os.Stat(files[j])
		return left.ModTime().After(right.ModTime())
	})
	if len(files) > 20 {
		files = files[:20]
	}
	for _, path := range files {
		entries := readJSONLines(path)
		if !copilotSessionMatches(entries, repository) {
			continue
		}
		if value, ok := copilotTurnFromEntries(entries, prompt); ok {
			value.TranscriptPath = path
			return value, true
		}
	}
	return turn{}, false
}

func copilotTurnFromFile(path, prompt string) (turn, bool) {
	return copilotTurnFromEntries(readJSONLines(path), prompt)
}

func copilotTurnFromEntries(entries []map[string]any, expectedPrompt string) (turn, bool) {
	assistants := []map[string]any{}
	for _, entry := range entries {
		data := mapValue(entry["data"])
		if stringValue(entry["type"]) == "assistant.message" && stringValue(data["content"]) != "" {
			assistants = append(assistants, entry)
		}
	}
	if len(assistants) == 0 {
		return turn{}, false
	}
	lastAssistant := assistants[len(assistants)-1]
	data := mapValue(lastAssistant["data"])
	interaction := stringValue(data["interactionId"])
	userPrompt := ""
	for _, entry := range entries {
		item := mapValue(entry["data"])
		if stringValue(entry["type"]) == "user.message" && stringValue(item["content"]) != "" && (interaction == "" || stringValue(item["interactionId"]) == interaction) {
			userPrompt = stringValue(item["content"])
		}
	}
	if expectedPrompt != "" && userPrompt != "" && userPrompt != expectedPrompt && !strings.Contains(userPrompt, expectedPrompt) {
		return turn{}, false
	}
	response := stringValue(data["content"])
	if response == "" {
		return turn{}, false
	}
	dedupe := firstNonEmpty(stringValue(data["messageId"]), stringValue(lastAssistant["id"]), sha1Hex(response))
	tools := []string{}
	seen := map[string]bool{}
	for _, assistant := range assistants {
		if requests, ok := mapValue(assistant["data"])["toolRequests"].([]any); ok {
			for _, request := range requests {
				name := stringValue(mapValue(request)["name"])
				if name != "" && !seen[name] {
					seen[name] = true
					tools = append(tools, name)
				}
			}
		}
	}
	return turn{DedupeKey: dedupe, LastAssistant: response, UserMessages: nonEmptySlice(userPrompt), AssistantMessages: []string{response}, ToolNames: tools}, true
}

func copilotSessionMatches(entries []map[string]any, repository string) bool {
	for _, entry := range entries {
		if stringValue(entry["type"]) == "session.start" {
			context := mapValue(mapValue(entry["data"])["context"])
			candidate := firstNonEmpty(stringValue(context["gitRoot"]), stringValue(context["cwd"]))
			if candidate == "" {
				return false
			}
			left, _ := filepath.Abs(candidate)
			right, _ := filepath.Abs(repository)
			return left == right
		}
	}
	return false
}

func markCopilotIngested(state string, rt *runtime, sessionKey, repository, dedupe, prompt, cwd string) {
	if sessionKey == "" || dedupe == "" {
		return
	}
	_ = updateState(sessionPath(state, sessionKey), rt.nowUTC(), map[string]any{"cwd": cwd, "repo_root": repository, "last_ingested_turn_id": dedupe, "last_ingested_at": rt.nowUTC().Format(timeFormat), "last_prompt": prompt})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func nonEmptySlice(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []string{strings.TrimSpace(value)}
}
