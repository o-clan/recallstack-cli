package hooks

import (
	"path/filepath"
	"regexp"
	"strings"
)

// While a hold is open, shell commands are allowed only when every part of
// them is a recallstack command or a read-only command from a fixed list, with
// no writes (redirects, command substitution, sequencing into other commands).

type shellSegment struct {
	words []string
}

// splitShell breaks a command line into pipeline/&& segments. ok is false when
// the line uses anything the gate refuses outright: `;`, `&`, newlines,
// command substitution, process substitution, or redirects other than to
// /dev/null or another descriptor.
func splitShell(command string) ([]shellSegment, bool) {
	segments := []shellSegment{}
	current := []string{}
	word := strings.Builder{}
	hasWord := false
	flushWord := func() {
		if hasWord {
			current = append(current, word.String())
			word.Reset()
			hasWord = false
		}
	}
	flushSegment := func() bool {
		flushWord()
		if len(current) == 0 {
			return false
		}
		segments = append(segments, shellSegment{words: current})
		current = []string{}
		return true
	}
	runes := []rune(command)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		next := rune(0)
		if index+1 < len(runes) {
			next = runes[index+1]
		}
		switch {
		case r == '\\' && next != 0:
			word.WriteRune(next)
			hasWord = true
			index++
		case r == '\'':
			end := index + 1
			for end < len(runes) && runes[end] != '\'' {
				end++
			}
			if end >= len(runes) {
				return nil, false
			}
			word.WriteString(string(runes[index+1 : end]))
			hasWord = true
			index = end
		case r == '"':
			end := index + 1
			for end < len(runes) && runes[end] != '"' {
				if runes[end] == '\\' && end+1 < len(runes) {
					end += 2
					continue
				}
				if runes[end] == '`' || (runes[end] == '$' && end+1 < len(runes) && runes[end+1] == '(') {
					return nil, false
				}
				end++
			}
			if end >= len(runes) {
				return nil, false
			}
			word.WriteString(strings.ReplaceAll(string(runes[index+1:end]), `\"`, `"`))
			hasWord = true
			index = end
		case r == '`', r == '$' && next == '(', (r == '<' || r == '>') && next == '(':
			return nil, false
		case r == ';', r == '\n', r == '\r':
			return nil, false
		case r == '|':
			if next == '|' {
				index++
			}
			if !flushSegment() {
				return nil, false
			}
		case r == '&':
			if next == '&' {
				index++
				if !flushSegment() {
					return nil, false
				}
				continue
			}
			return nil, false
		case r == '>' || r == '<':
			// Allow only harmless redirects: N>/dev/null, N>&M, &>/dev/null.
			flushWord()
			if r == '<' {
				return nil, false
			}
			if len(current) > 0 && regexp.MustCompile(`^[0-9]$`).MatchString(current[len(current)-1]) {
				current = current[:len(current)-1]
			}
			index++
			if index < len(runes) && runes[index] == '>' {
				index++
			}
			if index < len(runes) && runes[index] == '&' {
				index++
				start := index
				for index < len(runes) && runes[index] >= '0' && runes[index] <= '9' {
					index++
				}
				if index == start {
					return nil, false
				}
				index--
				continue
			}
			for index < len(runes) && runes[index] == ' ' {
				index++
			}
			start := index
			for index < len(runes) && !strings.ContainsRune(" \t|&;", runes[index]) {
				index++
			}
			if string(runes[start:index]) != "/dev/null" {
				return nil, false
			}
			index--
		case r == ' ' || r == '\t':
			flushWord()
		default:
			word.WriteRune(r)
			hasWord = true
		}
	}
	if !flushSegment() && len(segments) == 0 {
		return nil, false
	}
	return segments, true
}

var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// shellAllowedWhileHolding is the gate's decision for one shell command.
func shellAllowedWhileHolding(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return true
	}
	segments, ok := splitShell(command)
	if !ok {
		return false
	}
	for _, segment := range segments {
		words := segment.words
		for len(words) > 0 && envAssignment.MatchString(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 || !readOnlyCommand(words) {
			return false
		}
	}
	return true
}

func readOnlyCommand(words []string) bool {
	name := filepath.Base(words[0])
	args := words[1:]
	switch name {
	case "recallstack", "recallstack.exe":
		return true
	case "ls", "cat", "head", "tail", "pwd", "wc", "grep", "egrep", "fgrep":
		return true
	case "rg":
		for _, arg := range args {
			if arg == "--pre" || strings.HasPrefix(arg, "--pre=") {
				return false
			}
		}
		return true
	case "find":
		for _, arg := range args {
			switch arg {
			case "-exec", "-execdir", "-delete", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls":
				return false
			}
		}
		return true
	case "git":
		return readOnlyGit(args)
	}
	return false
}

func readOnlyGit(args []string) bool {
	for len(args) > 0 {
		switch {
		case args[0] == "-C" || args[0] == "-c":
			if len(args) < 2 || args[0] == "-c" {
				return false
			}
			args = args[2:]
		case args[0] == "--no-pager" || args[0] == "-P":
			args = args[1:]
		default:
			goto subcommand
		}
	}
subcommand:
	if len(args) == 0 {
		return false
	}
	sub, rest := args[0], args[1:]
	for _, arg := range rest {
		if arg == "--output" || strings.HasPrefix(arg, "--output=") || arg == "--ext-diff" {
			return false
		}
	}
	switch sub {
	case "status", "diff", "log", "show":
		return true
	case "branch":
		for _, arg := range rest {
			switch arg {
			case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--list", "-l", "--show-current", "--no-color", "--color", "--merged", "--no-merged", "--contains", "--sort":
				continue
			}
			if strings.HasPrefix(arg, "--sort=") || strings.HasPrefix(arg, "--format=") || strings.HasPrefix(arg, "--color=") {
				continue
			}
			return false
		}
		return true
	}
	return false
}

// toolGated reports whether a tool call should be denied while holding.
func toolGated(agent, tool string, input map[string]any) bool {
	lower := strings.ToLower(strings.TrimSpace(tool))
	switch agent {
	case "claude":
		switch tool {
		case "Edit", "Write", "MultiEdit", "NotebookEdit":
			return true
		case "Bash":
			return !shellAllowedWhileHolding(shellCommand(input))
		}
		return strings.HasPrefix(tool, "mcp__")
	case "codex":
		if isShellTool(lower) {
			return !shellAllowedWhileHolding(shellCommand(input))
		}
		return strings.Contains(lower, "patch") || lower == "edit" || lower == "write" || strings.HasPrefix(lower, "mcp__")
	case "copilot":
		if isShellTool(lower) {
			return !shellAllowedWhileHolding(shellCommand(input))
		}
		switch lower {
		case "edit", "create", "write", "str_replace_editor", "apply_patch", "multiedit", "notebookedit", "delete":
			return true
		}
		return strings.Contains(lower, "edit") || strings.Contains(lower, "write") || strings.HasPrefix(lower, "mcp")
	case "cursor":
		return !shellAllowedWhileHolding(shellCommand(input))
	}
	return false
}

func isShellTool(lower string) bool {
	switch lower {
	case "bash", "shell", "exec_command", "local_shell", "shell_command", "unified_exec", "container.exec", "powershell", "run_in_terminal", "runinterminal":
		return true
	}
	return false
}

func shellCommand(input map[string]any) string {
	for _, key := range []string{"command", "cmd", "script"} {
		switch value := input[key].(type) {
		case string:
			return value
		case []any:
			parts := make([]string, 0, len(value))
			for _, item := range value {
				if text, ok := item.(string); ok {
					parts = append(parts, text)
				}
			}
			// ["bash", "-lc", "<script>"] → the script.
			if len(parts) >= 3 && (parts[1] == "-lc" || parts[1] == "-c") {
				return parts[len(parts)-1]
			}
			return strings.Join(parts, " ")
		}
	}
	return ""
}
