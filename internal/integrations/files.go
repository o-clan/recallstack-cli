package integrations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type object = map[string]any

// ParseError is a config file Recallstack can't read. It is never overwritten.
type ParseError struct {
	Path   string
	Line   int
	Format string
	Err    error
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s isn't valid %s (line %d)", e.Path, e.Format, e.Line)
	}
	return fmt.Sprintf("%s isn't valid %s", e.Path, e.Format)
}

// WriteError is a config file Recallstack couldn't write.
type WriteError struct {
	Path string
	Err  error
}

func (e *WriteError) Error() string { return fmt.Sprintf("couldn't write %s: %v", e.Path, e.Err) }

func readJSON(path string) (object, bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return object{}, false, nil
	}
	if err != nil {
		return nil, true, &WriteError{Path: path, Err: err}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return object{}, true, nil
	}
	var value object
	if err := json.Unmarshal(body, &value); err != nil {
		line := 0
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			line = bytes.Count(body[:min(int(syntax.Offset), len(body))], []byte("\n")) + 1
		}
		return nil, true, &ParseError{Path: path, Line: line, Format: "JSON", Err: err}
	}
	if value == nil {
		value = object{}
	}
	return value, true, nil
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(body, '\n'), 0o644)
}

func writeFile(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &WriteError{Path: path, Err: err}
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		return &WriteError{Path: path, Err: err}
	}
	return nil
}

func readText(path string) (string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", &WriteError{Path: path, Err: err}
	}
	return string(body), nil
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

// removeCommands drops entries (and nested hook entries) whose field matches.
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
				copyItem := make(object, len(item))
				for key, value := range item {
					copyItem[key] = value
				}
				copyItem["hooks"] = next
				item = copyItem
			}
		}
		remaining = append(remaining, item)
	}
	return remaining, changed
}

func entriesContain(entries []any, field string, matches func(string) bool) bool {
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if matches(strings.TrimSpace(stringField(item, field))) {
			return true
		}
		if entriesContain(asSlice(item["hooks"]), field, matches) {
			return true
		}
	}
	return false
}

func isLegacyRecallstackCommand(command string) bool {
	normalized := strings.ToLower(strings.TrimSpace(command))
	return strings.Contains(normalized, "recallstack.mjs") ||
		strings.Contains(normalized, "recallstack.sh") ||
		strings.Contains(normalized, ".codex/hooks/log-hook.sh")
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
	insertAt := end
	for insertAt > start+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	lines = append(lines[:insertAt], append([]string{assignment}, lines[insertAt:]...)...)
	return normalizeToml(lines)
}

func removeTomlKey(content, section, key string) string {
	lines := splitNormalizedLines(content)
	start, end := tomlSectionRange(lines, section)
	if start < 0 {
		return content
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
			// Drop the empty section and the blank line before it.
			cut := newStart
			if cut > 0 && strings.TrimSpace(next[cut-1]) == "" {
				cut--
			}
			next = append(next[:cut], next[newEnd:]...)
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
	pattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=\s*` + regexp.QuoteMeta(expected) + `\s*(#.*)?$`)
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
