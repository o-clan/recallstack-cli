package hooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func queueRetry(rt *runtime, agent, state, cwd string, record map[string]any, result CommandResult) error {
	policy := currentRetryPolicy()
	now := rt.nowUTC()
	createdAt := stringValue(record["created_at"])
	if createdAt == "" {
		createdAt = now.Format(time.RFC3339Nano)
	}
	attempt := numberValue(record["attempt_count"]) + 1
	retryID := stringValue(record["retry_id"])
	if retryID == "" {
		retryID = sha1Hex(stringValue(record["idempotency_key"]))
	}
	record = mergeMap(record, map[string]any{
		"retry_id":        retryID,
		"created_at":      createdAt,
		"expires_at":      now.Add(policy.Window).Format(time.RFC3339Nano),
		"attempt_count":   attempt,
		"next_attempt_at": now.Add(policy.delay(attempt)).Format(time.RFC3339Nano),
		"last_error":      map[string]any{"status": result.Status, "stdout": truncate(result.Stdout, 1200), "stderr": truncate(result.Stderr, 1200)},
	})
	path := filepath.Join(state, "retry", retryID+".json")
	if err := writeJSON(path, record); err != nil {
		return err
	}
	appendEvent(state, now, "retry_queued", map[string]any{"retry_path": path, "next_attempt_at": record["next_attempt_at"]})
	pid, err := rt.spawnSelf(agent, "retry", path, cwd)
	if err == nil {
		appendEvent(state, now, "retry_worker_spawned", map[string]any{"retry_path": path, "pid": pid})
	}
	return err
}

func processRetry(ctx context.Context, rt *runtime, agent, path string, mark func(map[string]any)) error {
	record := readJSONMap(path)
	if len(record) == 0 {
		return nil
	}
	cwd := stringValue(record["cwd"])
	if cwd == "" {
		cwd = rt.cwd
	}
	state := stateDir(cwd, agent)
	policy := currentRetryPolicy()
	for {
		record = readJSONMap(path)
		if len(record) == 0 {
			return nil
		}
		now := rt.nowUTC()
		if expires, err := time.Parse(time.RFC3339Nano, stringValue(record["expires_at"])); err == nil && !now.Before(expires) {
			appendEvent(state, now, "retry_expired", map[string]any{"retry_path": path})
			removeFile(path)
			return nil
		}
		if next, err := time.Parse(time.RFC3339Nano, stringValue(record["next_attempt_at"])); err == nil && next.After(now) {
			if err := rt.sleep(ctx, next.Sub(now)); err != nil {
				return err
			}
			continue
		}
		target := stringValue(record["target"])
		summary := stringValue(record["summary"])
		idempotencyKey := stringValue(record["idempotency_key"])
		if target == "" || summary == "" || idempotencyKey == "" {
			removeFile(path)
			return fmt.Errorf("%s retry job is missing ingest payload", agent)
		}
		result := rt.ingest(ctx, cwd, target, summary, idempotencyKey, mapValue(record["metadata"]))
		if result.Status == 0 {
			mark(record)
			appendEvent(state, now, "retry_ingested", map[string]any{"retry_path": path, "target": target})
			removeFile(path)
			return nil
		}
		if !isRetryable(result) {
			appendError(state, now, "Retry abandoned after non-retryable ingest failure.", map[string]any{"retry_path": path, "status": result.Status, "stdout": truncate(result.Stdout, 1200), "stderr": truncate(result.Stderr, 1200)})
			removeFile(path)
			return nil
		}
		attempt := numberValue(record["attempt_count"]) + 1
		record["attempt_count"] = attempt
		record["next_attempt_at"] = now.Add(policy.delay(attempt)).Format(time.RFC3339Nano)
		record["last_error"] = map[string]any{"status": result.Status, "stdout": truncate(result.Stdout, 1200), "stderr": truncate(result.Stderr, 1200)}
		if err := writeJSON(path, record); err != nil {
			return err
		}
		if err := rt.sleep(ctx, policy.delay(attempt)); err != nil {
			return err
		}
	}
}

func removeJob(path string) { _ = os.Remove(path) }
