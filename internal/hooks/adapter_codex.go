package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type codexAdapter struct{}

func (codexAdapter) run(_ context.Context, rt *runtime, event string, payload map[string]any) error {
	cwd := stringValue(payload["cwd"])
	if cwd == "" {
		cwd = rt.cwd
	}
	state := stateDir(cwd, "codex")
	normalized := strings.ToLower(strings.ReplaceAll(event, "_", ""))
	if normalized == "" {
		normalized = strings.ToLower(strings.ReplaceAll(stringValue(payload["hook_event_name"]), "_", ""))
	}
	if normalized == "sessionstart" {
		sessionID := stringValue(payload["session_id"])
		if sessionID == "" {
			return nil
		}
		return updateState(sessionPath(state, sessionID), rt.nowUTC(), map[string]any{
			"session_id": sessionID, "transcript_path": nullableString(payload["transcript_path"]), "cwd": cwd,
			"source": nullableString(payload["source"]), "started_at": rt.nowUTC().Format(timeFormat),
		})
	}
	if normalized != "stop" {
		return nil
	}
	sessionID := stringValue(payload["session_id"])
	transcript := stringValue(payload["transcript_path"])
	if sessionID == "" || transcript == "" {
		return nil
	}
	path, err := enqueueJob(rt, "codex", cwd, state, payload, nil)
	_ = updateState(sessionPath(state, sessionID), rt.nowUTC(), map[string]any{"session_id": sessionID, "transcript_path": transcript, "cwd": cwd, "last_stop_queued_at": rt.nowUTC().Format(timeFormat), "last_stop_job_id": filepathBaseWithoutExt(path)})
	return err
}

func (codexAdapter) worker(ctx context.Context, rt *runtime, jobPath string) error {
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
	state := stateDir(cwd, "codex")
	target, ok := rt.resolveProject(ctx, cwd, state)
	if !ok {
		return nil
	}
	var value turn
	for attempt := 0; attempt < 40; attempt++ {
		value, ok = latestCodexTurn(transcript)
		if ok {
			break
		}
		if err := rt.sleep(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	if !ok {
		appendError(state, rt.nowUTC(), "Stop worker could not find a completed turn in transcript.", map[string]any{"job_path": jobPath, "transcript_path": transcript, "session_id": sessionID})
		return nil
	}
	statePath := sessionPath(state, sessionID)
	session := readJSONMap(statePath)
	turnKey := value.TurnID
	if turnKey == "" {
		turnKey = value.DedupeKey
	}
	if stringValue(session["last_processed_turn_id"]) == value.DedupeKey {
		appendEvent(state, rt.nowUTC(), "job_skipped_duplicate", map[string]any{"session_id": sessionID, "turn_id": turnKey})
		return nil
	}
	extracted := runCodexExtractor(ctx, rt, "codex", cwd, state, value, session["previous_turn_snapshot"])
	_ = updateState(statePath, rt.nowUTC(), map[string]any{"session_id": sessionID, "transcript_path": transcript, "cwd": cwd, "last_processed_turn_id": value.DedupeKey, "last_processed_at": rt.nowUTC().Format(timeFormat)})
	if extracted.TurnKind == "pleasantry" {
		return nil
	}
	summary := formatSummary(extracted)
	if summary == "" {
		return nil
	}
	_ = updateState(statePath, rt.nowUTC(), map[string]any{"previous_turn_snapshot": turnSnapshot(extracted, summary)})
	metadata := extractionMetadata("codex", "codex_hook", "codex_exec", sessionID, transcript, extracted, session["previous_turn_snapshot"] != nil)
	metadata["turn_id"] = nullableText(value.TurnID)
	idempotencyKey := "codex-hook-" + sha1Hex(sessionID+":"+turnKey)
	result := rt.ingest(ctx, cwd, target, summary, idempotencyKey, metadata)
	if result.Status != 0 {
		if isRetryable(result) {
			return queueRetry(rt, "codex", state, cwd, map[string]any{"cwd": cwd, "target": target, "summary": summary, "metadata": metadata, "idempotency_key": idempotencyKey, "session_id": sessionID, "dedupe_key": value.DedupeKey, "turn_key": turnKey}, result)
		}
		appendError(state, rt.nowUTC(), "Recallstack ingest failed from Codex Stop worker.", map[string]any{"status": result.Status, "stdout": truncate(result.Stdout, 1200), "stderr": truncate(result.Stderr, 1200)})
		return nil
	}
	markCodexIngested(state, rt, sessionID, turnKey)
	appendEvent(state, rt.nowUTC(), "job_ingested", map[string]any{"session_id": sessionID, "target": target, "dedupe_key": value.DedupeKey})
	return nil
}

func (codexAdapter) retry(ctx context.Context, rt *runtime, path string) error {
	return processRetry(ctx, rt, "codex", path, func(record map[string]any) {
		cwd := stringValue(record["cwd"])
		markCodexIngested(stateDir(cwd, "codex"), rt, stringValue(record["session_id"]), stringValue(record["turn_key"]))
	})
}

func markCodexIngested(state string, rt *runtime, sessionID, turnKey string) {
	if sessionID == "" || turnKey == "" {
		return
	}
	_ = updateState(sessionPath(state, sessionID), rt.nowUTC(), map[string]any{"last_ingested_turn_id": turnKey, "last_ingested_at": rt.nowUTC().Format(timeFormat)})
}

func latestCodexTurn(path string) (turn, bool) {
	entries := readJSONLines(path)
	lastAssistant := -1
	for index := len(entries) - 1; index >= 0; index-- {
		payload := mapValue(entries[index]["payload"])
		if stringValue(entries[index]["type"]) == "response_item" && stringValue(payload["type"]) == "message" && stringValue(payload["role"]) == "assistant" && textContent(payload["content"], "input_text", "output_text") != "" {
			lastAssistant = index
			break
		}
	}
	if lastAssistant < 0 {
		return turn{}, false
	}
	previous := -1
	for index := lastAssistant - 1; index >= 0; index-- {
		payload := mapValue(entries[index]["payload"])
		if stringValue(entries[index]["type"]) == "response_item" && stringValue(payload["type"]) == "message" && stringValue(payload["role"]) == "assistant" && textContent(payload["content"], "input_text", "output_text") != "" {
			previous = index
			break
		}
	}
	value := turn{DedupeKey: fmt.Sprintf("assistant-index:%d", lastAssistant)}
	if timestamp := stringValue(entries[lastAssistant]["timestamp"]); timestamp != "" {
		value.DedupeKey = "assistant:" + timestamp
	}
	for _, entry := range entries[previous+1:] {
		payload := mapValue(entry["payload"])
		if stringValue(entry["type"]) == "response_item" {
			if stringValue(payload["type"]) == "reasoning" {
				continue
			}
			if stringValue(payload["type"]) == "message" {
				text := textContent(payload["content"], "input_text", "output_text")
				role := stringValue(payload["role"])
				if text != "" {
					kind := "message"
					if role == "user" {
						kind = "user_message"
						value.UserMessages = append(value.UserMessages, text)
					}
					if role == "assistant" {
						kind = "assistant_message"
						value.AssistantMessages = append(value.AssistantMessages, text)
					}
					value.Items = append(value.Items, map[string]any{"kind": kind, "role": role, "phase": nullableString(payload["phase"]), "text": text})
				}
			} else {
				encoded, _ := json.Marshal(payload)
				value.Items = append(value.Items, map[string]any{"kind": "response_item", "item_type": stringValue(payload["type"]), "summary": truncate(string(encoded), 1200)})
			}
		}
		if stringValue(entry["type"]) == "event_msg" && stringValue(payload["type"]) == "task_complete" {
			if id := stringValue(payload["turn_id"]); id != "" {
				value.TurnID = id
				value.DedupeKey = id
			}
			value.Items = append(value.Items, map[string]any{"kind": "task_complete", "turn_id": nullableString(payload["turn_id"]), "last_agent_message": nullableString(payload["last_agent_message"])})
		}
		if stringValue(entry["type"]) == "event_msg" && stringValue(payload["type"]) == "task_started" {
			if value.TurnID == "" {
				value.TurnID = stringValue(payload["turn_id"])
			}
			value.Items = append(value.Items, map[string]any{"kind": "task_started", "turn_id": nullableString(payload["turn_id"])})
		}
	}
	value.LastAssistant = last(value.AssistantMessages)
	return value, true
}

func nullableString(value any) any {
	if text := stringValue(value); text != "" {
		return text
	}
	return nil
}

func nullableText(value string) any {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return nil
}
