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

type antigravityAdapter struct{}

func (antigravityAdapter) run(_ context.Context, rt *runtime, event string, payload map[string]any) error {
	defer func() {
		encoded, _ := json.Marshal(map[string]any{"decision": "allow"})
		_, _ = fmt.Fprintln(rt.stdout, string(encoded))
	}()
	event = strings.ToLower(firstNonEmpty(event, stringValue(payload["hookEventName"]), stringValue(payload["hook_event_name"]), "stop"))
	if event != "stop" {
		return nil
	}
	cwd := antigravityCWD(payload, rt.cwd)
	state := stateDir(cwd, "antigravity")
	_, err := enqueueJob(rt, "antigravity", cwd, state, payload, map[string]any{"raw_payload": payload})
	return err
}

func (antigravityAdapter) worker(ctx context.Context, rt *runtime, jobPath string) error {
	job := readJSONMap(jobPath)
	if len(job) == 0 {
		return nil
	}
	cwd := firstNonEmpty(stringValue(job["cwd"]), rt.cwd)
	state := stateDir(cwd, "antigravity")
	payload := mapValue(job["raw_payload"])
	if len(payload) == 0 {
		payload = mapValue(job["payload"])
	}
	statePath := antigravitySessionPath(state, payload)
	session := readJSONMap(statePath)
	target, ok := rt.resolveProject(ctx, cwd, state)
	if !ok {
		appendEvent(state, rt.nowUTC(), "project_target_missing", map[string]any{"job_path": jobPath})
		return nil
	}
	turns := antigravityTurns(payload, session)
	if len(turns) == 0 {
		appendEvent(state, rt.nowUTC(), "turn_skipped", map[string]any{"job_path": jobPath, "reason": "no_new_turns"})
		removeJob(jobPath)
		return nil
	}
	processed, skipped := 0, 0
	lastStep := numberValue(session["last_ingested_step_index"])
	if _, found := session["last_ingested_step_index"]; !found {
		lastStep = -1
	}
	previous := session["last_turn_snapshot"]
	for _, value := range turns {
		extracted := runCodexExtractor(ctx, rt, "antigravity", cwd, state, value, previous)
		step := value.AssistantStepIndex
		if step < value.UserStepIndex {
			step = value.UserStepIndex
		}
		if extracted.TurnKind == "pleasantry" {
			skipped++
			if step > lastStep {
				lastStep = step
			}
			continue
		}
		summary := formatSummary(extracted)
		if summary == "" {
			skipped++
			if step > lastStep {
				lastStep = step
			}
			continue
		}
		workspacePaths := payload["workspacePaths"]
		if workspacePaths == nil {
			workspacePaths = []string{}
		}
		metadata := map[string]any{"agent": "google_antigravity", "source": "antigravity_stop_hook", "extractor": "codex_json_schema_worker", "turn_kind": extracted.TurnKind, "user_intent": extracted.UserIntent, "agent_intent": extracted.AgentIntent, "key_actions": extracted.KeyActions, "tradeoffs": extracted.Tradeoffs, "open_questions": extracted.OpenQuestions, "conversation_id": nullableString(payload["conversationId"]), "transcript_path": nullableText(value.TranscriptPath), "user_step_index": nullableIndex(value.UserStepIndex), "assistant_step_index": nullableIndex(value.AssistantStepIndex), "termination_reason": nullableString(payload["terminationReason"]), "fully_idle": payload["fullyIdle"], "workspace_paths": workspacePaths}
		idempotencyKey := "antigravity-hook-" + sha1Hex(cwd+":"+value.DedupeKey)
		result := rt.ingest(ctx, cwd, target, summary, idempotencyKey, metadata)
		if result.Status != 0 {
			appendError(state, rt.nowUTC(), "Recallstack ingest failed from Antigravity hook.", map[string]any{"status": result.Status, "stdout": truncate(result.Stdout, 1200), "stderr": truncate(result.Stderr, 1200), "target": target, "job_path": jobPath})
			_ = updateState(statePath, rt.nowUTC(), map[string]any{"last_ingested_step_index": lastStep})
			return nil
		}
		processed++
		previous = turnSnapshot(extracted, summary)
		if step > lastStep {
			lastStep = step
		}
	}
	_ = updateState(statePath, rt.nowUTC(), map[string]any{"cwd": cwd, "transcript_path": nullableString(payload["transcriptPath"]), "last_turn_snapshot": previous, "last_ingested_step_index": lastStep})
	if processed == 0 && skipped > 0 {
		appendEvent(state, rt.nowUTC(), "turn_skipped", map[string]any{"job_path": jobPath, "reason": "low_signal_or_empty", "skipped_count": skipped})
		removeJob(jobPath)
		return nil
	}
	appendEvent(state, rt.nowUTC(), "job_ingested", map[string]any{"target": target, "job_path": jobPath, "processed_count": processed, "skipped_count": skipped})
	removeJob(jobPath)
	return nil
}

func (antigravityAdapter) retry(context.Context, *runtime, string) error { return nil }

func antigravityCWD(payload map[string]any, fallback string) string {
	if paths, ok := payload["workspacePaths"].([]any); ok {
		for _, path := range paths {
			if value := stringValue(path); value != "" {
				return value
			}
		}
	}
	return firstNonEmpty(stringValue(payload["cwd"]), fallback)
}

func antigravitySessionPath(state string, payload map[string]any) string {
	key := stringValue(payload["conversationId"])
	if key == "" {
		key = sha1Hex(firstNonEmpty(stringValue(payload["transcriptPath"]), "unknown"))
	}
	return sessionPath(state, key)
}

var userRequestPattern = regexp.MustCompile(`(?is)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)
var additionalMetadataPattern = regexp.MustCompile(`(?is)<ADDITIONAL_METADATA>.*$`)

func antigravityTurns(payload, session map[string]any) []turn {
	transcript := stringValue(payload["transcriptPath"])
	entries := readJSONLines(transcript)
	lastStep := numberValue(session["last_ingested_step_index"])
	if _, found := session["last_ingested_step_index"]; !found {
		lastStep = -1
	}
	values := []turn{}
	var current *turn
	flush := func() {
		if current != nil && last(current.UserMessages) != "" && last(current.AssistantMessages) != "" {
			current.TranscriptPath = transcript
			current.DedupeKey = sha1Hex(firstNonEmpty(stringValue(payload["conversationId"]), "") + "\n" + transcript + "\n" + fmt.Sprint(current.UserStepIndex) + "\n" + fmt.Sprint(current.AssistantStepIndex) + "\n" + last(current.UserMessages) + "\n" + last(current.AssistantMessages))
			values = append(values, *current)
		}
	}
	for index, entry := range entries {
		step := numberValue(entry["step_index"])
		if _, found := entry["step_index"]; !found {
			step = index
		}
		if step <= lastStep {
			continue
		}
		role := antigravityRole(entry)
		if role == "user" || stringValue(entry["source"]) == "USER_EXPLICIT" || stringValue(entry["type"]) == "USER_INPUT" {
			flush()
			raw := firstNonEmpty(anyText(entry["userMessage"], 0), anyText(entry["user_message"], 0), anyText(entry["prompt"], 0), anyText(entry["input"], 0), anyText(entry["content"], 0), anyText(entry, 0))
			text := normalizeAntigravityUser(raw)
			if text != "" {
				current = &turn{UserMessages: []string{truncate(text, 1000)}, UserStepIndex: step, AssistantStepIndex: step}
			} else {
				current = nil
			}
			continue
		}
		assistant := ""
		if stringValue(entry["source"]) == "MODEL" && stringValue(entry["type"]) == "PLANNER_RESPONSE" {
			assistant = stringValue(entry["content"])
		} else if role == "assistant" {
			assistant = anyText(entry, 0)
		}
		if current != nil && assistant != "" {
			current.AssistantMessages = []string{truncate(assistant, 1400)}
			current.LastAssistant = truncate(assistant, 1400)
			current.AssistantStepIndex = step
		}
	}
	flush()
	return values
}

func antigravityRole(entry map[string]any) string {
	candidates := []string{stringValue(entry["role"]), stringValue(entry["type"]), stringValue(mapValue(entry["author"])["role"]), stringValue(mapValue(entry["message"])["role"]), stringValue(entry["sender"]), stringValue(entry["source"])}
	value := strings.ToLower(strings.Join(candidates, " "))
	if strings.Contains(value, "user") || strings.Contains(value, "human") {
		return "user"
	}
	if strings.Contains(value, "assistant") || strings.Contains(value, "agent") || strings.Contains(value, "model") {
		return "assistant"
	}
	return ""
}

func normalizeAntigravityUser(value string) string {
	value = strings.TrimSpace(value)
	if match := userRequestPattern.FindStringSubmatch(value); len(match) > 1 {
		return compactWhitespace(match[1], 1000)
	}
	return strings.TrimSpace(additionalMetadataPattern.ReplaceAllString(value, ""))
}
func nullableIndex(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

var _ = os.ErrNotExist
var _ = filepath.Separator
