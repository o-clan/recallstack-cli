package integrations

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Codex 0.162 runs a hook only after the person has trusted it once in
// Codex's /hooks screen. Trusting writes, into the user config.toml,
//
//	[hooks.state."{hooks.json path}:{event}:{group}:{handler}"]
//	trusted_hash = "sha256:…"
//
// where the hash covers the normalized hook (codex-rs/hooks discovery.rs,
// hook_hash). A hook whose hash no longer matches (a changed command, for
// example) counts as modified and needs trusting again. Recallstack only reads
// this state; it never writes trust entries.

// CodexHookState is one installed Recallstack hook as Codex sees it.
type CodexHookState struct {
	Event  string `json:"event"`
	Key    string `json:"key"`
	Status string `json:"status"` // trusted | untrusted | modified | disabled
}

// CodexTrust is the trust state of Recallstack's Codex hooks in one hooks.json.
type CodexTrust struct {
	HooksPath  string           `json:"hooks_path"`
	ConfigPath string           `json:"config_path"`
	Hooks      []CodexHookState `json:"hooks"`
	Waiting    int              `json:"waiting"`  // untrusted or modified
	Disabled   int              `json:"disabled"` // turned off in /hooks
	// RepoTrusted is false when a repo install sits in a folder Codex doesn't
	// trust, so Codex never reads its .codex/ at all.
	RepoTrusted bool `json:"repo_trusted"`
}

// Ready reports whether Codex will run every Recallstack hook.
func (t CodexTrust) Ready() bool { return t.Waiting == 0 && t.Disabled == 0 && t.RepoTrusted }

// CodexHome is Codex's config folder ($CODEX_HOME, else ~/.codex).
func (m *Manager) CodexHome() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		return value
	}
	return filepath.Join(m.Home, ".codex")
}

var codexEventLabels = map[string]string{
	"SessionStart": "session_start", "UserPromptSubmit": "user_prompt_submit", "PreToolUse": "pre_tool_use",
	"PostToolUse": "post_tool_use", "Stop": "stop", "SessionEnd": "session_end",
}

// CodexHooksTrust reads Codex's own trust state for the Recallstack hooks in
// scope's hooks.json. ok is false when none are installed there.
func (m *Manager) CodexHooksTrust(scope Scope) (CodexTrust, bool) {
	hooksPath := filepath.Join(m.root(scope, ".codex"), "hooks.json")
	if scope == ScopeGlobal {
		hooksPath = filepath.Join(m.CodexHome(), "hooks.json")
	}
	return m.codexTrustFor(hooksPath, scope == ScopeLocal)
}

func (m *Manager) codexTrustFor(hooksPath string, repo bool) (CodexTrust, bool) {
	trust := CodexTrust{HooksPath: hooksPath, ConfigPath: filepath.Join(m.CodexHome(), "config.toml"), RepoTrusted: true}
	config, _, err := readJSON(hooksPath)
	if err != nil {
		return trust, false
	}
	toml, _ := readText(trust.ConfigPath)
	entries := parseTomlEntries(toml)
	keys := []string{hooksPath}
	if resolved, err := filepath.EvalSymlinks(hooksPath); err == nil && resolved != hooksPath {
		keys = append(keys, resolved)
	}
	owned := ownedCommand(TargetCodex)
	hooks := asObject(config["hooks"])
	for _, item := range codexEvents {
		groups := asSlice(hooks[item.event])
		for groupIndex, rawGroup := range groups {
			group := asObject(rawGroup)
			for handlerIndex, rawHandler := range asSlice(group["hooks"]) {
				handler := asObject(rawHandler)
				command, _ := handler["command"].(string)
				if !owned(command) {
					continue
				}
				current := codexHookHash(item.event, group, handler)
				state := CodexHookState{Event: item.event, Status: "untrusted"}
				for _, source := range keys {
					key := source + ":" + codexEventLabels[item.event] + ":" + strconv.Itoa(groupIndex) + ":" + strconv.Itoa(handlerIndex)
					if state.Key == "" {
						state.Key = key
					}
					hashed, _ := entries.get("hooks", "state", key, "trusted_hash").(string)
					enabled, hasEnabled := entries.get("hooks", "state", key, "enabled").(bool)
					if hashed == "" && !hasEnabled {
						continue
					}
					state.Key = key
					switch {
					case hasEnabled && !enabled:
						state.Status = "disabled"
					case hashed == current:
						state.Status = "trusted"
					case hashed != "":
						state.Status = "modified"
					}
					break
				}
				trust.Hooks = append(trust.Hooks, state)
				switch state.Status {
				case "untrusted", "modified":
					trust.Waiting++
				case "disabled":
					trust.Disabled++
				}
			}
		}
	}
	if len(trust.Hooks) == 0 {
		return trust, false
	}
	if repo {
		trust.RepoTrusted = codexTrustsFolder(entries, filepath.Dir(filepath.Dir(hooksPath)))
	}
	return trust, true
}

// codexTrustsFolder: Codex reads a repo's .codex/ only when that folder, or one
// above it, is trusted in the user config.
func codexTrustsFolder(entries tomlEntries, folder string) bool {
	folder = filepath.Clean(folder)
	candidates := []string{folder}
	if resolved, err := filepath.EvalSymlinks(folder); err == nil && resolved != folder {
		candidates = append(candidates, resolved)
	}
	for _, start := range candidates {
		for dir := start; ; dir = filepath.Dir(dir) {
			if level, _ := entries.get("projects", dir, "trust_level").(string); level != "" {
				return level == "trusted"
			}
			if parent := filepath.Dir(dir); parent == dir {
				break
			}
		}
	}
	return false
}

// codexHookHash reproduces Codex 0.162's hook_hash: sha256 over the
// key-sorted compact JSON of {event_name, matcher, hooks: [normalized handler]}.
func codexHookHash(event string, group, handler object) string {
	identity := map[string]any{"event_name": codexEventLabels[event]}
	if event != "UserPromptSubmit" && event != "Stop" {
		if matcher, ok := group["matcher"].(string); ok {
			identity["matcher"] = matcher
		}
	}
	normalized := map[string]any{"type": "command", "command": handler["command"]}
	async, _ := handler["async"].(bool)
	normalized["async"] = async
	timeout := int64(0)
	hasTimeout := false
	if value, ok := handler["timeout"].(float64); ok {
		timeout, hasTimeout = int64(value), true
	}
	switch event {
	case "SessionEnd":
		if !hasTimeout {
			timeout = 1
		}
		if timeout < 1 {
			timeout = 1
		}
		if timeout > 3 {
			timeout = 3
		}
	default:
		if !hasTimeout {
			timeout = 600
		}
		if timeout < 1 {
			timeout = 1
		}
	}
	normalized["timeout"] = timeout
	if message, ok := handler["statusMessage"].(string); ok {
		normalized["statusMessage"] = message
	}
	switch event {
	case "PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit":
		if limit, ok := handler["additionalContextLimit"].(float64); ok && int64(limit) != 2500 {
			normalized["additionalContextLimit"] = int64(limit)
		}
	}
	identity["hooks"] = []any{normalized}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(identity)
	sum := sha256.Sum256(bytes.TrimRight(buffer.Bytes(), "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---- a small TOML reader, enough for Codex's hook state and project trust ----

type tomlEntries map[string]any

func tomlPathKey(parts []string) string { return strings.Join(parts, "\x00") }

func (e tomlEntries) get(parts ...string) any {
	if value, ok := e[tomlPathKey(parts)]; ok {
		return value
	}
	// Inline table one level up: "key" = { trusted_hash = "…" }.
	if len(parts) > 1 {
		if table, ok := e[tomlPathKey(parts[:len(parts)-1])].(map[string]any); ok {
			return table[parts[len(parts)-1]]
		}
	}
	return nil
}

// parseTomlEntries reads tables, dotted and quoted keys, strings, booleans,
// numbers and inline tables. Arrays and multi-line strings are skipped.
func parseTomlEntries(content string) tomlEntries {
	out := tomlEntries{}
	table := []string{}
	skip := false
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[[") {
			skip = true // arrays of tables aren't needed here
			continue
		}
		if strings.HasPrefix(line, "[") {
			keys, rest, ok := parseTomlKey(line[1:])
			skip = !ok || !strings.HasPrefix(strings.TrimSpace(rest), "]")
			table = keys
			continue
		}
		keys, rest, ok := parseTomlKey(line)
		rest = strings.TrimSpace(rest)
		if !ok || !strings.HasPrefix(rest, "=") {
			continue
		}
		raw := strings.TrimSpace(rest[1:])
		// Skip values that continue on later lines.
		if strings.HasPrefix(raw, `"""`) || strings.HasPrefix(raw, "'''") {
			quote := raw[:3]
			if strings.Count(raw, quote) < 2 {
				for index++; index < len(lines) && !strings.Contains(lines[index], quote); index++ {
				}
			}
			continue
		}
		if strings.HasPrefix(raw, "[") {
			depth := strings.Count(raw, "[") - strings.Count(raw, "]")
			for depth > 0 && index+1 < len(lines) {
				index++
				depth += strings.Count(lines[index], "[") - strings.Count(lines[index], "]")
			}
			continue
		}
		if skip {
			continue
		}
		value, _, ok := parseTomlValue(raw)
		if !ok {
			continue
		}
		full := append(append([]string{}, table...), keys...)
		out[tomlPathKey(full)] = value
	}
	return out
}

// parseTomlKey reads a dotted key (bare, "basic" or 'literal' parts) and
// returns what follows it.
func parseTomlKey(input string) ([]string, string, bool) {
	keys := []string{}
	rest := strings.TrimLeft(input, " \t")
	for {
		var part string
		var ok bool
		switch {
		case strings.HasPrefix(rest, `"`):
			part, rest, ok = parseBasicString(rest)
		case strings.HasPrefix(rest, "'"):
			end := strings.Index(rest[1:], "'")
			if end < 0 {
				return nil, "", false
			}
			part, rest, ok = rest[1:1+end], rest[2+end:], true
		default:
			end := 0
			for end < len(rest) && (isBareKeyChar(rest[end])) {
				end++
			}
			if end == 0 {
				return nil, "", false
			}
			part, rest, ok = rest[:end], rest[end:], true
		}
		if !ok {
			return nil, "", false
		}
		keys = append(keys, part)
		rest = strings.TrimLeft(rest, " \t")
		if !strings.HasPrefix(rest, ".") {
			return keys, rest, true
		}
		rest = strings.TrimLeft(rest[1:], " \t")
	}
}

func isBareKeyChar(c byte) bool {
	return c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func parseBasicString(input string) (string, string, bool) {
	var builder strings.Builder
	for index := 1; index < len(input); index++ {
		c := input[index]
		switch c {
		case '"':
			return builder.String(), input[index+1:], true
		case '\\':
			if index+1 >= len(input) {
				return "", "", false
			}
			index++
			switch input[index] {
			case 'n':
				builder.WriteByte('\n')
			case 't':
				builder.WriteByte('\t')
			case 'r':
				builder.WriteByte('\r')
			case 'b':
				builder.WriteByte('\b')
			case 'f':
				builder.WriteByte('\f')
			case 'u', 'U':
				size := 4
				if input[index] == 'U' {
					size = 8
				}
				if index+size >= len(input) {
					return "", "", false
				}
				code, err := strconv.ParseUint(input[index+1:index+1+size], 16, 32)
				if err != nil {
					return "", "", false
				}
				builder.WriteRune(rune(code))
				index += size
			default:
				builder.WriteByte(input[index])
			}
		default:
			builder.WriteByte(c)
		}
	}
	return "", "", false
}

func parseTomlValue(input string) (any, string, bool) {
	input = strings.TrimLeft(input, " \t")
	switch {
	case strings.HasPrefix(input, `"`):
		value, rest, ok := parseBasicString(input)
		return value, rest, ok
	case strings.HasPrefix(input, "'"):
		end := strings.Index(input[1:], "'")
		if end < 0 {
			return nil, "", false
		}
		return input[1 : 1+end], input[2+end:], true
	case strings.HasPrefix(input, "true"):
		return true, input[4:], true
	case strings.HasPrefix(input, "false"):
		return false, input[5:], true
	case strings.HasPrefix(input, "{"):
		table := map[string]any{}
		rest := strings.TrimLeft(input[1:], " \t")
		for {
			if strings.HasPrefix(rest, "}") {
				return table, rest[1:], true
			}
			keys, after, ok := parseTomlKey(rest)
			after = strings.TrimLeft(after, " \t")
			if !ok || !strings.HasPrefix(after, "=") {
				return nil, "", false
			}
			value, next, ok := parseTomlValue(after[1:])
			if !ok {
				return nil, "", false
			}
			table[strings.Join(keys, ".")] = value
			rest = strings.TrimLeft(next, " \t")
			if strings.HasPrefix(rest, ",") {
				rest = strings.TrimLeft(rest[1:], " \t")
			}
		}
	}
	end := 0
	for end < len(input) && input[end] != ' ' && input[end] != '\t' && input[end] != '#' && input[end] != ',' && input[end] != '}' {
		end++
	}
	if end == 0 {
		return nil, "", false
	}
	if number, err := strconv.ParseFloat(strings.ReplaceAll(input[:end], "_", ""), 64); err == nil {
		return number, input[end:], true
	}
	return input[:end], input[end:], true
}
