package hooks

import (
	"path/filepath"
	"time"
)

func sessionPath(state, id string) string {
	return filepath.Join(state, "sessions", id+".json")
}

func conversationPath(state, id string) string {
	return filepath.Join(state, "conversations", id+".json")
}

func updateState(path string, now time.Time, updates map[string]any) error {
	current := readJSONMap(path)
	updates["updated_at"] = now.UTC().Format(time.RFC3339Nano)
	return writeJSON(path, mergeMap(current, updates))
}

func enqueueJob(rt *runtime, agent, cwd, state string, payload map[string]any, extra map[string]any) (string, error) {
	id := jobID(rt.nowUTC(), agent+":"+stringValue(payload["session_id"])+":"+stringValue(payload["conversation_id"])+":"+stringValue(payload["conversationId"]))
	path := filepath.Join(state, "jobs", id+".json")
	job := map[string]any{"job_id": id, "queued_at": rt.nowUTC().Format(time.RFC3339Nano), "cwd": cwd, "payload": payload}
	for key, value := range extra {
		job[key] = value
	}
	if err := writeJSON(path, job); err != nil {
		return "", err
	}
	pid, err := rt.spawnSelf(agent, "worker", path, cwd)
	if err == nil {
		appendEvent(state, rt.nowUTC(), "job_spawned", map[string]any{"job_path": path, "pid": pid})
	}
	return path, err
}
