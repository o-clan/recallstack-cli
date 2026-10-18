package hooks

import (
	"crypto/sha1" // Idempotency keys keep today's {agent}-{sha1(session_id:turn_key)} shape.
	"encoding/hex"
	"encoding/json"
	"strings"
)

func sha1Hex(parts ...string) string {
	digest := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(digest[:])
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
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

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}

func compactWhitespace(value string, limit int) string {
	return truncate(strings.Join(strings.Fields(value), " "), limit)
}

func compact(value string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(value), " "))
}
