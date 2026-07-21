package integrations

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type object = map[string]any

func readJSON(path string) (object, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return object{}, nil
	}
	if err != nil {
		return nil, err
	}
	var value object
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if value == nil {
		value = object{}
	}
	return value, nil
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFile(path, body, 0o644)
}

func writeFile(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, mode)
}

func readText(path string) (string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(body), err
}

func remove(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, os.RemoveAll(path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func pruneEmpty(paths []string, stopAt string) {
	stop := filepath.Clean(stopAt)
	for _, path := range paths {
		cursor := filepath.Clean(path)
		for cursor != stop && pathWithin(cursor, stop) {
			entries, err := os.ReadDir(cursor)
			if errors.Is(err, os.ErrNotExist) {
				cursor = filepath.Dir(cursor)
				continue
			}
			if err != nil || len(entries) != 0 {
				break
			}
			if os.Remove(cursor) != nil {
				break
			}
			cursor = filepath.Dir(cursor)
		}
	}
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func asObject(value any) object {
	if typed, ok := value.(map[string]any); ok && typed != nil {
		return typed
	}
	return object{}
}

func asSlice(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return nil
}

func stringField(value any, key string) string {
	item, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	text, _ := item[key].(string)
	return text
}

func removeCommands(entries []any, command func(string) bool, field string) ([]any, bool) {
	changed := false
	remaining := make([]any, 0, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			remaining = append(remaining, entry)
			continue
		}
		if command(stringField(item, field)) {
			changed = true
			continue
		}
		if hooks, ok := item["hooks"].([]any); ok {
			next, nestedChanged := removeCommands(hooks, command, field)
			changed = changed || nestedChanged
			if len(next) == 0 && nestedChanged {
				continue
			}
			if nestedChanged {
				copyItem := cloneObject(item)
				copyItem["hooks"] = next
				item = copyItem
			}
		}
		remaining = append(remaining, item)
	}
	return remaining, changed
}

func cloneObject(value object) object {
	next := make(object, len(value))
	for key, item := range value {
		next[key] = item
	}
	return next
}

func isLegacyRecallstackCommand(command string) bool {
	normalized := strings.ToLower(strings.TrimSpace(command))
	return strings.Contains(normalized, "recallstack.mjs") ||
		strings.Contains(normalized, "recallstack.sh") ||
		strings.Contains(normalized, ".codex/hooks/log-hook.sh")
}

func isNativeRecallstackCommand(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	return len(fields) >= 4 && fields[0] == "recallstack" && fields[1] == "hook" && fields[2] == "run"
}

func upsertTomlKey(content, section, key, value string) string {
	lines := splitNormalizedLines(content)
	start, end := tomlSectionRange(lines, section)
	assignment := key + " = " + value
	if start < 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "["+section+"]", assignment)
		return normalizeToml(lines)
	}
	pattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for index := start + 1; index < end; index++ {
		if pattern.MatchString(lines[index]) {
			lines[index] = assignment
			return normalizeToml(lines)
		}
	}
	lines = append(lines, "")
	copy(lines[end+1:], lines[end:])
	lines[end] = assignment
	return normalizeToml(lines)
}

func removeTomlKey(content, section, key string) string {
	lines := splitNormalizedLines(content)
	start, end := tomlSectionRange(lines, section)
	if start < 0 {
		return normalizeToml(lines)
	}
	pattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	next := append([]string{}, lines[:start+1]...)
	for _, line := range lines[start+1 : end] {
		if !pattern.MatchString(line) {
			next = append(next, line)
		}
	}
	next = append(next, lines[end:]...)
	newStart, newEnd := tomlSectionRange(next, section)
	if newStart >= 0 {
		hasContent := false
		for _, line := range next[newStart+1 : newEnd] {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				hasContent = true
				break
			}
		}
		if !hasContent {
			next = append(next[:newStart], next[newEnd:]...)
		}
	}
	return normalizeToml(next)
}

func tomlKeyEnabled(content, section, key, expected string) bool {
	lines := splitNormalizedLines(content)
	start, end := tomlSectionRange(lines, section)
	if start < 0 {
		return false
	}
	pattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=\s*` + regexp.QuoteMeta(expected) + `\s*$`)
	for _, line := range lines[start+1 : end] {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

func splitNormalizedLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func tomlSectionRange(lines []string, section string) (int, int) {
	header := "[" + section + "]"
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == header {
			start = index
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	sectionPattern := regexp.MustCompile(`^\s*\[.*\]\s*$`)
	for index := start + 1; index < len(lines); index++ {
		if sectionPattern.MatchString(lines[index]) {
			return start, index
		}
	}
	return start, len(lines)
}

func normalizeToml(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func trimOutput(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit-3] + "..."
}

const (
	managedInstructionsStart = "<!-- recallstack:instructions:start -->"
	managedInstructionsEnd   = "<!-- recallstack:instructions:end -->"
)

func mergeManagedInstructions(current string, instructions []byte) string {
	body := strings.TrimSpace(string(instructions))
	managed := managedInstructionsStart + "\n" + body + "\n" + managedInstructionsEnd
	trimmed := strings.TrimSpace(current)
	// The TypeScript CLI owned the whole file. Convert that exact legacy output
	// to a managed section so future installs can coexist with repository guidance.
	if trimmed == body || trimmed == "" {
		return managed + "\n"
	}
	start := strings.Index(current, managedInstructionsStart)
	end := strings.Index(current, managedInstructionsEnd)
	if start >= 0 && end >= start {
		end += len(managedInstructionsEnd)
		return current[:start] + managed + current[end:]
	}
	separator := "\n\n"
	if strings.HasSuffix(current, "\n\n") {
		separator = ""
	} else if strings.HasSuffix(current, "\n") {
		separator = "\n"
	}
	return current + separator + managed + "\n"
}

func removeManagedInstructions(current string) (string, bool) {
	start := strings.Index(current, managedInstructionsStart)
	end := strings.Index(current, managedInstructionsEnd)
	if start < 0 || end < start {
		// Support uninstalling the exact whole-file output from the TypeScript CLI.
		if strings.TrimSpace(current) == strings.TrimSpace(string(copilotInstructions)) {
			return "", true
		}
		return current, false
	}
	end += len(managedInstructionsEnd)
	next := current[:start] + current[end:]
	next = strings.TrimSpace(next)
	if next == "" {
		return "", true
	}
	return next + "\n", true
}
