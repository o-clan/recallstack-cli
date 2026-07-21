package hooks

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

func readJSONLines(path string) []map[string]any {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	entries := make([]map[string]any, 0)
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 8*1024*1024)
	for scanner.Scan() {
		var entry map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(scanner.Text())), &entry) == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

func textContent(value any, allowedTypes ...string) string {
	if direct, ok := value.(string); ok {
		return strings.TrimSpace(direct)
	}
	allowed := map[string]bool{}
	for _, item := range allowedTypes {
		allowed[item] = true
	}
	var values []any
	switch typed := value.(type) {
	case []any:
		values = typed
	default:
		return ""
	}
	parts := make([]string, 0)
	for _, item := range values {
		entry := mapValue(item)
		kind := stringValue(entry["type"])
		if len(allowed) == 0 || allowed[kind] {
			if text := stringValue(entry["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func anyText(value any, depth int) string {
	if depth > 6 || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := anyText(item, depth+1); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case map[string]any:
		parts := make([]string, 0)
		for _, key := range []string{"text", "content", "message", "parts", "result", "response", "output", "input", "prompt"} {
			if item, found := typed[key]; found {
				if text := anyText(item, depth+1); text != "" {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) == 0 {
			for _, item := range typed {
				if text := anyText(item, depth+1); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}
