package hooks

import (
	"crypto/sha1" // Idempotency compatibility with the previous hook runtime.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

const timeFormat = time.RFC3339Nano

func ensureDir(path string) error { return os.MkdirAll(path, 0o755) }

func readJSON(path string, fallback any) any {
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, fallback) != nil {
		return fallback
	}
	return fallback
}

func readJSONMap(path string) map[string]any {
	value := map[string]any{}
	readJSON(path, &value)
	return value
}

func writeJSON(path string, value any) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".recallstack-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if goruntime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	return os.Rename(temporaryPath, path)
}

func appendLog(stateDir, filename string, now time.Time, entry map[string]any) {
	if ensureDir(stateDir) != nil {
		return
	}
	value := map[string]any{"timestamp": now.UTC().Format(time.RFC3339Nano)}
	for key, item := range entry {
		value[key] = item
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(stateDir, filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(data, '\n'))
}

func appendEvent(stateDir string, now time.Time, event string, extra map[string]any) {
	appendLog(stateDir, "events.log", now, map[string]any{"event": event, "extra": extra})
}

func appendError(stateDir string, now time.Time, message string, extra map[string]any) {
	appendLog(stateDir, "errors.log", now, map[string]any{"message": message, "extra": extra})
}

func sha1Hex(parts ...string) string {
	digest := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(digest[:])
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func numberValue(value any) int {
	switch item := value.(type) {
	case float64:
		return int(item)
	case int:
		return item
	case json.Number:
		value, _ := item.Int64()
		return int(value)
	default:
		return 0
	}
}

func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if direct, ok := value.([]string); ok {
			return direct
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text := stringValue(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}

func mergeMap(base map[string]any, updates map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(updates))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range updates {
		result[key] = value
	}
	return result
}

func compactWhitespace(value string, limit int) string {
	return truncate(strings.Join(strings.Fields(value), " "), limit)
}

func compact(value string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(value), " "))
}

func removeFile(path string) { _ = os.Remove(path) }

func jobID(now time.Time, seed string) string {
	return sha1Hex(fmt.Sprintf("%s:%d", seed, now.UnixNano()))
}

func filepathBaseWithoutExt(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func extractionMetadata(agent, source, extractor, sessionID, transcript string, value extractedTurn, previous bool) map[string]any {
	return map[string]any{
		"agent": agent, "source": source, "extractor": extractor,
		"session_id": sessionID, "transcript_path": transcript,
		"previous_turn_snapshot_used": previous, "turn_kind": value.TurnKind,
		"user_intent": value.UserIntent, "agent_intent": value.AgentIntent,
		"key_actions": value.KeyActions, "tradeoffs": value.Tradeoffs,
		"open_questions": value.OpenQuestions,
	}
}
