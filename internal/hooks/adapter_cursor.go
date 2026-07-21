package hooks

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type cursorAdapter struct{}

func (cursorAdapter) run(_ context.Context, rt *runtime, event string, payload map[string]any) error {
	if os.Getenv("RECALLSTACK_HOOK_ACTIVE") == "1" {
		return nil
	}
	if event == "" {
		event = stringValue(payload["hook_event_name"])
	}
	cwd := rt.cwd
	if roots, ok := payload["workspace_roots"].([]any); ok {
		for _, root := range roots {
			if value := stringValue(root); value != "" {
				cwd = value
				break
			}
		}
	}
	state := stateDir(cwd, "cursor")
	conversationID := stringValue(payload["conversation_id"])
	generationID := stringValue(payload["generation_id"])
	if conversationID == "" {
		return nil
	}
	path := conversationPath(state, conversationID)
	switch event {
	case "beforeSubmitPrompt":
		prompt := firstString(payload, "prompt", "text")
		if prompt != "" {
			_ = updateState(path, rt.nowUTC(), map[string]any{"cwd": cwd, "last_prompt": prompt, "last_prompt_generation_id": nullableText(generationID), "last_prompt_at": rt.nowUTC().Format(timeFormat)})
			appendEvent(state, rt.nowUTC(), "prompt_captured", map[string]any{"conversation_id": conversationID, "generation_id": nullableText(generationID)})
		}
	case "afterAgentResponse":
		response := firstString(payload, "text", "response", "agent_message", "message")
		if response != "" {
			_ = updateState(path, rt.nowUTC(), map[string]any{"cwd": cwd, "last_response": response, "last_response_generation_id": nullableText(generationID), "last_response_at": rt.nowUTC().Format(timeFormat)})
			appendEvent(state, rt.nowUTC(), "response_captured", map[string]any{"conversation_id": conversationID, "generation_id": nullableText(generationID)})
		}
	case "stop":
		return enqueueCursorStop(rt, cwd, state, conversationID, generationID, payload)
	}
	return nil
}

func enqueueCursorStop(rt *runtime, cwd, state, conversationID, generationID string, payload map[string]any) error {
	path := conversationPath(state, conversationID)
	session := readJSONMap(path)
	dedupe := generationID
	if dedupe == "" {
		dedupe = stringValue(session["last_response_generation_id"])
	}
	if dedupe == "" {
		dedupe = stringValue(session["last_prompt_generation_id"])
	}
	if dedupe == "" {
		dedupe = conversationID
	}
	if stringValue(session["last_ingested_generation_id"]) == dedupe || stringValue(session["last_queued_generation_id"]) == dedupe {
		appendEvent(state, rt.nowUTC(), "stop_duplicate_skipped", map[string]any{"conversation_id": conversationID, "generation_id": dedupe})
		return nil
	}
	prompt := stringValue(session["last_prompt"])
	response := stringValue(session["last_response"])
	if prompt == "" && response == "" {
		return nil
	}
	jobPath, err := enqueueJob(rt, "cursor", cwd, state, payload, map[string]any{
		"conversation_id": conversationID, "generation_id": dedupe, "prompt": prompt, "response": response,
		"previous_turn_snapshot": session["previous_turn_snapshot"], "raw_payload": payload,
	})
	_ = updateState(path, rt.nowUTC(), map[string]any{"cwd": cwd, "last_queued_generation_id": dedupe, "last_queued_at": rt.nowUTC().Format(timeFormat)})
	if err == nil {
		appendEvent(state, rt.nowUTC(), "job_spawned", map[string]any{"conversation_id": conversationID, "generation_id": dedupe, "job_path": jobPath})
	}
	return err
}

func (cursorAdapter) worker(ctx context.Context, rt *runtime, jobPath string) error {
	job := readJSONMap(jobPath)
	if len(job) == 0 {
		return nil
	}
	cwd := stringValue(job["cwd"])
	if cwd == "" {
		cwd = rt.cwd
	}
	state := stateDir(cwd, "cursor")
	target, ok := rt.resolveProject(ctx, cwd, state)
	if !ok {
		return nil
	}
	prompt := stringValue(job["prompt"])
	if pleasantryPattern.MatchString(compactWhitespace(prompt, 400)) {
		appendEvent(state, rt.nowUTC(), "job_noop", map[string]any{"job_path": jobPath})
		removeJob(jobPath)
		return nil
	}
	response := stringValue(job["response"])
	intent := cursorIntent(prompt, mapValue(job["previous_turn_snapshot"]))
	lines := []string{}
	if intent != "" {
		lines = append(lines, "User intent: "+intent)
	}
	if response != "" {
		lines = append(lines, "Outcome: "+compactWhitespace(response, 3200))
	}
	summary := strings.TrimSpace(strings.Join(lines, "\n"))
	if summary == "" {
		removeJob(jobPath)
		return nil
	}
	conversationID := stringValue(job["conversation_id"])
	generationID := stringValue(job["generation_id"])
	_ = updateState(conversationPath(state, conversationID), rt.nowUTC(), map[string]any{"previous_turn_snapshot": map[string]any{"turn_kind": "thin", "user_intent": intent, "agent_intent": "", "outcome": compactWhitespace(response, 600), "summary": compactWhitespace(summary, 900)}})
	metadata := map[string]any{"agent": "cursor_ide", "source": "cursor_stop_hook", "extractor": "cursor_hook_pairing", "conversation_id": nullableText(conversationID), "generation_id": nullableText(generationID), "previous_turn_snapshot_used": job["previous_turn_snapshot"] != nil, "prompt": nullableText(compactWhitespace(prompt, 4000))}
	idempotencyKey := "cursor-hook-" + sha1Hex(conversationID+":"+generationID)
	result := rt.ingest(ctx, cwd, target, summary, idempotencyKey, metadata)
	if result.Status != 0 {
		if isRetryable(result) {
			return queueRetry(rt, "cursor", state, cwd, map[string]any{"cwd": cwd, "target": target, "summary": summary, "metadata": metadata, "idempotency_key": idempotencyKey, "conversation_id": conversationID, "generation_id": generationID}, result)
		}
		appendError(state, rt.nowUTC(), "Recallstack ingest failed from Cursor Stop hook.", map[string]any{"status": result.Status, "stderr": truncate(result.Stderr, 1200)})
		return nil
	}
	markCursorIngested(state, rt, conversationID, generationID, cwd)
	appendEvent(state, rt.nowUTC(), "job_ingested", map[string]any{"conversation_id": conversationID, "generation_id": generationID, "target": target, "job_path": jobPath})
	removeJob(jobPath)
	return nil
}

func (cursorAdapter) retry(ctx context.Context, rt *runtime, path string) error {
	return processRetry(ctx, rt, "cursor", path, func(record map[string]any) {
		cwd := stringValue(record["cwd"])
		markCursorIngested(stateDir(cwd, "cursor"), rt, stringValue(record["conversation_id"]), stringValue(record["generation_id"]), cwd)
	})
}

func markCursorIngested(state string, rt *runtime, conversationID, generationID, cwd string) {
	if conversationID == "" || generationID == "" {
		return
	}
	_ = updateState(conversationPath(state, conversationID), rt.nowUTC(), map[string]any{"cwd": cwd, "last_ingested_generation_id": generationID, "last_ingested_at": rt.nowUTC().Format(timeFormat)})
}

var referentialPattern = regexp.MustCompile(`(?i)^(let'?s go with|go with|option\s+\d+|do that|use that|go ahead|that works|sounds good|let'?s do that|the first one|the second one|the third one|that one|this one|proceed with that)\b`)

func cursorIntent(prompt string, previous map[string]any) string {
	latest := compactWhitespace(prompt, 1600)
	if latest == "" || len(previous) == 0 || !referentialPattern.MatchString(latest) {
		return latest
	}
	context := compactWhitespace(stringValue(previous["user_intent"]), 800)
	if context == "" {
		context = compactWhitespace(stringValue(previous["outcome"]), 800)
	}
	if context == "" {
		return latest
	}
	return compactWhitespace(latest+" (referring to previous turn: "+context+")", 1600)
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if item := stringValue(value[key]); item != "" {
			return item
		}
	}
	return ""
}

var _ = filepath.Separator
