package hooks

import (
	"context"
	"fmt"
	"os"
	"strings"
)

type claudeAdapter struct{}

func (claudeAdapter) run(_ context.Context, rt *runtime, event string, payload map[string]any) error {
	if os.Getenv("RECALLSTACK_HOOK_ACTIVE") == "1" {
		return nil
	}
	if event == "" {
		event = stringValue(payload["hook_event_name"])
	}
	if strings.ToLower(event) != "stop" {
		return nil
	}
	cwd := stringValue(payload["cwd"])
	if cwd == "" {
		cwd = rt.cwd
	}
	sessionID := stringValue(payload["session_id"])
	transcript := stringValue(payload["transcript_path"])
	if sessionID == "" || transcript == "" {
		return nil
	}
	state := stateDir(cwd, "claude")
	path, err := enqueueJob(rt, "claude", cwd, state, payload, nil)
	_ = updateState(sessionPath(state, sessionID), rt.nowUTC(), map[string]any{
		"session_id": sessionID, "transcript_path": transcript, "cwd": cwd,
		"last_stop_queued_at": rt.nowUTC().Format(timeFormat), "last_stop_job_id": filepathBaseWithoutExt(path),
	})
	return err
}

func (claudeAdapter) worker(ctx context.Context, rt *runtime, jobPath string) error {
	job := readJSONMap(jobPath)
	if len(job) == 0 {
		return nil
	}
	defer removeJob(jobPath)
	cwd := stringValue(job["cwd"])
	if cwd == "" {
		cwd = rt.cwd
	}
	payload := mapValue(job["payload"])
	sessionID := stringValue(payload["session_id"])
	transcript := stringValue(payload["transcript_path"])
	if sessionID == "" || transcript == "" {
		return nil
	}
	state := stateDir(cwd, "claude")
	target, ok := rt.resolveProject(ctx, cwd, state)
	if !ok {
		return nil
	}
	value, ok := latestClaudeTurn(transcript, payload)
	if !ok {
		appendError(state, rt.nowUTC(), "Claude Stop worker could not identify the latest completed turn.", map[string]any{"job_path": jobPath, "transcript_path": transcript, "session_id": sessionID})
		return nil
	}
	statePath := sessionPath(state, sessionID)
	session := readJSONMap(statePath)
	if stringValue(session["last_processed_turn_id"]) == value.DedupeKey {
		appendEvent(state, rt.nowUTC(), "job_skipped_duplicate", map[string]any{"job_path": jobPath, "session_id": sessionID, "dedupe_key": value.DedupeKey})
		return nil
	}
	extracted := runClaudeExtractor(ctx, rt, cwd, state, value, session["previous_turn_snapshot"])
	_ = updateState(statePath, rt.nowUTC(), map[string]any{"session_id": sessionID, "transcript_path": transcript, "cwd": cwd, "last_processed_turn_id": value.DedupeKey, "last_processed_at": rt.nowUTC().Format(timeFormat)})
	if extracted.TurnKind == "pleasantry" {
		appendEvent(state, rt.nowUTC(), "job_noop", map[string]any{"session_id": sessionID, "dedupe_key": value.DedupeKey})
		return nil
	}
	summary := formatSummary(extracted)
	if summary == "" {
		return nil
	}
	_ = updateState(statePath, rt.nowUTC(), map[string]any{"previous_turn_snapshot": turnSnapshot(extracted, summary)})
	metadata := extractionMetadata("claude_code", "claude_stop_hook", "claude_print", sessionID, transcript, extracted, session["previous_turn_snapshot"] != nil)
	idempotencyKey := "claude-hook-" + sha1Hex(sessionID+":"+value.DedupeKey)
	result := rt.ingest(ctx, cwd, target, summary, idempotencyKey, metadata)
	if result.Status != 0 {
		if isRetryable(result) {
			return queueRetry(rt, "claude", state, cwd, map[string]any{"cwd": cwd, "target": target, "summary": summary, "metadata": metadata, "idempotency_key": idempotencyKey, "session_id": sessionID, "dedupe_key": value.DedupeKey}, result)
		}
		appendError(state, rt.nowUTC(), "Recallstack ingest failed from Claude Stop worker.", map[string]any{"status": result.Status, "stdout": truncate(result.Stdout, 1200), "stderr": truncate(result.Stderr, 1200), "target": target})
		return nil
	}
	markClaudeIngested(state, rt, sessionID, value.DedupeKey)
	appendEvent(state, rt.nowUTC(), "job_ingested", map[string]any{"session_id": sessionID, "target": target, "dedupe_key": value.DedupeKey})
	return nil
}

func (claudeAdapter) retry(ctx context.Context, rt *runtime, path string) error {
	return processRetry(ctx, rt, "claude", path, func(record map[string]any) {
		cwd := stringValue(record["cwd"])
		markClaudeIngested(stateDir(cwd, "claude"), rt, stringValue(record["session_id"]), stringValue(record["dedupe_key"]))
	})
}

func markClaudeIngested(state string, rt *runtime, sessionID, dedupe string) {
	if sessionID == "" || dedupe == "" {
		return
	}
	_ = updateState(sessionPath(state, sessionID), rt.nowUTC(), map[string]any{"last_ingested_turn_id": dedupe, "last_ingested_at": rt.nowUTC().Format(timeFormat)})
}

func latestClaudeTurn(path string, payload map[string]any) (turn, bool) {
	entries := readJSONLines(path)
	lastAssistant := -1
	for index := len(entries) - 1; index >= 0; index-- {
		message := mapValue(entries[index]["message"])
		if stringValue(entries[index]["type"]) == "assistant" && stringValue(message["role"]) == "assistant" && textContent(message["content"], "text") != "" {
			lastAssistant = index
			break
		}
	}
	if lastAssistant < 0 {
		fallback := stringValue(payload["last_assistant_message"])
		if fallback == "" {
			return turn{}, false
		}
		dedupe := stringValue(payload["stop_hook_active"])
		if dedupe == "" {
			dedupe = "assistant:" + sha1Hex(fallback)
		}
		return turn{DedupeKey: dedupe, LastAssistant: fallback, AssistantMessages: []string{fallback}, Items: []any{map[string]any{"kind": "assistant_message", "text": fallback}}}, true
	}
	previous := -1
	for index := lastAssistant - 1; index >= 0; index-- {
		message := mapValue(entries[index]["message"])
		if stringValue(entries[index]["type"]) == "assistant" && stringValue(message["role"]) == "assistant" && textContent(message["content"], "text") != "" {
			previous = index
			break
		}
	}
	value := turn{}
	for _, entry := range entries[previous+1:] {
		message := mapValue(entry["message"])
		role := stringValue(message["role"])
		text := textContent(message["content"], "text")
		if role == "user" && text != "" {
			value.UserMessages = append(value.UserMessages, text)
			value.Items = append(value.Items, map[string]any{"kind": "user_message", "text": text})
		}
		if role == "assistant" {
			if text != "" {
				value.AssistantMessages = append(value.AssistantMessages, text)
				value.Items = append(value.Items, map[string]any{"kind": "assistant_message", "text": text})
			}
			if content, ok := message["content"].([]any); ok {
				for _, item := range content {
					entry := mapValue(item)
					if stringValue(entry["type"]) == "tool_use" && stringValue(entry["name"]) != "" {
						value.Items = append(value.Items, map[string]any{"kind": "tool_use", "name": stringValue(entry["name"])})
					}
				}
			}
		}
	}
	assistantEntry := entries[lastAssistant]
	value.DedupeKey = stringValue(assistantEntry["uuid"])
	if value.DedupeKey == "" {
		value.DedupeKey = stringValue(assistantEntry["timestamp"])
	}
	if value.DedupeKey == "" {
		value.DedupeKey = fmt.Sprintf("assistant-index:%d", lastAssistant)
	}
	value.LastAssistant = last(value.AssistantMessages)
	return value, true
}
