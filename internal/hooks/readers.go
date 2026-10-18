package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// turn is the newest completed turn read from an agent's own transcript. It
// never leaves the computer: only the extracted records do.
type turn struct {
	DedupeKey         string
	TurnID            string
	LastAssistant     string
	UserMessages      []string
	AssistantMessages []string
	ToolNames         []string
	// RecallstackCommands are the recallstack writes the agent ran this turn,
	// so the extractor doesn't record the same thought twice.
	RecallstackCommands []string
	TranscriptPath      string
	UserStepIndex       int
	AssistantStepIndex  int
}

var pleasantryPattern = regexp.MustCompile(`(?i)^(thanks|thank you|thx|ty|got it(?:,? thanks)?|okay,? thanks|ok,? thanks|cool,? thanks|nice,? thanks|appreciate it|sounds good,? thanks|perfect,? thanks|great,? thanks|ok|okay|cool|nice|great|perfect)[!. ]*$`)

func last(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return strings.TrimSpace(items[len(items)-1])
}

func latestClaudeTurn(path string, payload map[string]any) (turn, bool) {
	entries := readJSONLines(path)
	isAssistantText := func(entry map[string]any) bool {
		message := mapValue(entry["message"])
		return stringValue(entry["type"]) == "assistant" && stringValue(message["role"]) == "assistant" && textContent(message["content"], "text") != ""
	}
	lastAssistant := -1
	for index := len(entries) - 1; index >= 0; index-- {
		if isAssistantText(entries[index]) {
			lastAssistant = index
			break
		}
	}
	if lastAssistant < 0 {
		fallback := stringValue(payload["last_assistant_message"])
		if fallback == "" {
			return turn{}, false
		}
		return turn{DedupeKey: "assistant:" + sha1Hex(fallback), LastAssistant: fallback, AssistantMessages: []string{fallback}}, true
	}
	// The turn starts at the last real user prompt before the final answer.
	start := -1
	for index := lastAssistant - 1; index >= 0; index-- {
		message := mapValue(entries[index]["message"])
		if stringValue(message["role"]) == "user" && textContent(message["content"], "text") != "" && !isToolResult(message["content"]) {
			start = index
			break
		}
	}
	if start < 0 {
		start = 0
	}
	value := turn{}
	for _, entry := range entries[start : lastAssistant+1] {
		message := mapValue(entry["message"])
		role := stringValue(message["role"])
		text := textContent(message["content"], "text")
		if role == "user" && text != "" && !isToolResult(message["content"]) && !boolValue(entry["isMeta"]) {
			value.UserMessages = append(value.UserMessages, text)
		}
		if role == "assistant" {
			if text != "" {
				value.AssistantMessages = append(value.AssistantMessages, text)
			}
			if content, ok := message["content"].([]any); ok {
				for _, item := range content {
					block := mapValue(item)
					if stringValue(block["type"]) == "tool_use" && stringValue(block["name"]) != "" {
						value.ToolNames = append(value.ToolNames, stringValue(block["name"]))
						value.RecallstackCommands = append(value.RecallstackCommands, recallstackCommands(block["input"])...)
					}
				}
			}
		}
	}
	assistantEntry := entries[lastAssistant]
	value.DedupeKey = firstNonEmpty(stringValue(assistantEntry["uuid"]), stringValue(assistantEntry["timestamp"]), fmt.Sprintf("assistant-index:%d", lastAssistant))
	value.TurnID = value.DedupeKey
	value.LastAssistant = last(value.AssistantMessages)
	value.TranscriptPath = path
	return value, len(value.UserMessages) > 0 || value.LastAssistant != ""
}

func isToolResult(content any) bool {
	items, ok := content.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if stringValue(mapValue(item)["type"]) == "tool_result" {
			return true
		}
	}
	return false
}

// latestCodexTurn reads the newest turn of a Codex rollout. A turn runs from
// task_started to task_complete and can hold several assistant messages
// (commentary between tool calls), so turn events mark the boundaries.
func latestCodexTurn(path string) (turn, bool) {
	entries := readJSONLines(path)
	start, end := -1, len(entries)
	turnID := ""
	for index := len(entries) - 1; index >= 0; index-- {
		payload := mapValue(entries[index]["payload"])
		if stringValue(entries[index]["type"]) != "event_msg" {
			continue
		}
		kind := stringValue(payload["type"])
		if kind == "task_complete" && turnID == "" {
			turnID, end = stringValue(payload["turn_id"]), index
			continue
		}
		if kind == "task_started" && (turnID == "" || stringValue(payload["turn_id"]) == turnID) {
			if turnID == "" {
				// Started but not complete yet: the worker polls again.
				return turn{}, false
			}
			start = index
			break
		}
	}
	if start < 0 {
		return latestCodexTurnByMessages(entries, path)
	}
	value := turn{TurnID: turnID, DedupeKey: turnID, TranscriptPath: path}
	for _, entry := range entries[start:end] {
		payload := mapValue(entry["payload"])
		if stringValue(entry["type"]) != "response_item" {
			continue
		}
		switch stringValue(payload["type"]) {
		case "message":
			text := textContent(payload["content"], "input_text", "output_text")
			if text == "" {
				continue
			}
			switch stringValue(payload["role"]) {
			case "user":
				if !codexInjected(text) {
					value.UserMessages = append(value.UserMessages, text)
				}
			case "assistant":
				value.AssistantMessages = append(value.AssistantMessages, text)
			}
		case "function_call", "custom_tool_call", "local_shell_call":
			if name := stringValue(payload["name"]); name != "" {
				value.ToolNames = append(value.ToolNames, name)
			}
			value.RecallstackCommands = append(value.RecallstackCommands, recallstackCommands(payload)...)
		}
	}
	value.LastAssistant = last(value.AssistantMessages)
	return value, value.LastAssistant != "" || len(value.UserMessages) > 0
}

// latestCodexTurnByMessages is the fallback for rollouts without turn events.
func latestCodexTurnByMessages(entries []map[string]any, path string) (turn, bool) {
	isAssistant := func(entry map[string]any) bool {
		payload := mapValue(entry["payload"])
		return stringValue(entry["type"]) == "response_item" && stringValue(payload["type"]) == "message" && stringValue(payload["role"]) == "assistant" && textContent(payload["content"], "input_text", "output_text") != ""
	}
	lastAssistant := -1
	for index := len(entries) - 1; index >= 0; index-- {
		if isAssistant(entries[index]) {
			lastAssistant = index
			break
		}
	}
	if lastAssistant < 0 {
		return turn{}, false
	}
	start := 0
	for index := lastAssistant - 1; index >= 0; index-- {
		payload := mapValue(entries[index]["payload"])
		if stringValue(entries[index]["type"]) == "response_item" && stringValue(payload["role"]) == "user" && !codexInjected(textContent(payload["content"], "input_text")) {
			start = index
			break
		}
	}
	value := turn{DedupeKey: fmt.Sprintf("assistant-index:%d", lastAssistant), TranscriptPath: path}
	if timestamp := stringValue(entries[lastAssistant]["timestamp"]); timestamp != "" {
		value.DedupeKey = "assistant:" + timestamp
	}
	value.TurnID = value.DedupeKey
	for _, entry := range entries[start : lastAssistant+1] {
		payload := mapValue(entry["payload"])
		if stringValue(entry["type"]) != "response_item" || stringValue(payload["type"]) != "message" {
			continue
		}
		text := textContent(payload["content"], "input_text", "output_text")
		if text == "" {
			continue
		}
		if stringValue(payload["role"]) == "user" && !codexInjected(text) {
			value.UserMessages = append(value.UserMessages, text)
		}
		if stringValue(payload["role"]) == "assistant" {
			value.AssistantMessages = append(value.AssistantMessages, text)
		}
	}
	value.LastAssistant = last(value.AssistantMessages)
	return value, true
}

// codexInjected filters context Codex adds as user messages (AGENTS.md, env, hook context).
var recallstackWrite = regexp.MustCompile(`recallstack\s+(?:note|hold|resolve|adjust|drop|undo)\b(?:[^\n"\\]|\\.|"[^"\n]*")*`)

// recallstackCommands finds recallstack writes in a tool call's input, whatever
// shape the agent wraps the shell command in.
func recallstackCommands(input any) []string {
	raw, _ := json.Marshal(input)
	text := string(raw)
	var unquoted string
	if json.Unmarshal(raw, &unquoted) == nil {
		text = unquoted
	}
	text = strings.NewReplacer(`\"`, `"`, `\n`, "\n").Replace(text)
	out := []string{}
	for _, match := range recallstackWrite.FindAllString(text, 8) {
		match = strings.TrimSpace(match)
		if runes := []rune(match); len(runes) > 400 {
			match = string(runes[:400])
		}
		out = append(out, match)
	}
	return out
}

func codexInjected(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{"<environment_context>", "<user_instructions>", "# AGENTS.md instructions", "<recallstack_", "<permissions instructions>", "<collaboration_mode>", "<turn_aborted>"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func findCopilotTurn(payload map[string]any, repository, prompt string) (turn, bool) {
	for _, key := range []string{"transcript_path", "transcriptPath"} {
		if path := stringValue(payload[key]); path != "" {
			if value, ok := copilotTurnFromEntries(readJSONLines(path), prompt); ok {
				value.TranscriptPath = path
				return value, true
			}
		}
	}
	root := filepath.Join(userHome(), ".copilot", "session-state")
	files := []string{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool {
		left, _ := os.Stat(files[i])
		right, _ := os.Stat(files[j])
		return left != nil && right != nil && left.ModTime().After(right.ModTime())
	})
	if len(files) > 20 {
		files = files[:20]
	}
	for _, path := range files {
		entries := readJSONLines(path)
		if !copilotSessionMatches(entries, repository) {
			continue
		}
		if value, ok := copilotTurnFromEntries(entries, prompt); ok {
			value.TranscriptPath = path
			return value, true
		}
	}
	return turn{}, false
}

func copilotTurnFromEntries(entries []map[string]any, expectedPrompt string) (turn, bool) {
	assistants := []map[string]any{}
	for _, entry := range entries {
		data := mapValue(entry["data"])
		if stringValue(entry["type"]) == "assistant.message" && stringValue(data["content"]) != "" {
			assistants = append(assistants, entry)
		}
	}
	if len(assistants) == 0 {
		return turn{}, false
	}
	lastAssistant := assistants[len(assistants)-1]
	data := mapValue(lastAssistant["data"])
	interaction := stringValue(data["interactionId"])
	userPrompt := ""
	for _, entry := range entries {
		item := mapValue(entry["data"])
		if stringValue(entry["type"]) == "user.message" && stringValue(item["content"]) != "" && (interaction == "" || stringValue(item["interactionId"]) == interaction) {
			userPrompt = stringValue(item["content"])
		}
	}
	if expectedPrompt != "" && userPrompt != "" && userPrompt != expectedPrompt && !strings.Contains(userPrompt, expectedPrompt) {
		return turn{}, false
	}
	response := stringValue(data["content"])
	if response == "" {
		return turn{}, false
	}
	if userPrompt == "" {
		userPrompt = expectedPrompt
	}
	dedupe := firstNonEmpty(stringValue(data["messageId"]), stringValue(lastAssistant["id"]), sha1Hex(response))
	return turn{DedupeKey: dedupe, TurnID: dedupe, LastAssistant: response, UserMessages: nonEmptySlice(userPrompt), AssistantMessages: []string{response}}, true
}

func copilotSessionMatches(entries []map[string]any, repository string) bool {
	for _, entry := range entries {
		if stringValue(entry["type"]) == "session.start" {
			context := mapValue(mapValue(entry["data"])["context"])
			candidate := firstNonEmpty(stringValue(context["gitRoot"]), stringValue(context["cwd"]))
			if candidate == "" {
				return false
			}
			left, _ := filepath.Abs(candidate)
			right, _ := filepath.Abs(repository)
			return left == right
		}
	}
	return false
}

var userRequestPattern = regexp.MustCompile(`(?is)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)
var additionalMetadataPattern = regexp.MustCompile(`(?is)<ADDITIONAL_METADATA>.*$`)

// antigravityTurns returns the turns after lastStep in an Antigravity transcript.
func antigravityTurns(transcript, conversationID string, lastStep int) []turn {
	entries := readJSONLines(transcript)
	values := []turn{}
	var current *turn
	flush := func() {
		if current != nil && last(current.UserMessages) != "" && last(current.AssistantMessages) != "" {
			current.TranscriptPath = transcript
			current.DedupeKey = sha1Hex(conversationID + "\n" + transcript + "\n" + fmt.Sprint(current.UserStepIndex) + "\n" + fmt.Sprint(current.AssistantStepIndex))
			current.TurnID = current.DedupeKey
			values = append(values, *current)
		}
	}
	for index, entry := range entries {
		step := numberValue(entry["step_index"])
		if _, found := entry["step_index"]; !found {
			step = index
		}
		if step <= lastStep {
			continue
		}
		role := antigravityRole(entry)
		if role == "user" || stringValue(entry["source"]) == "USER_EXPLICIT" || stringValue(entry["type"]) == "USER_INPUT" {
			flush()
			raw := firstNonEmpty(anyText(entry["userMessage"], 0), anyText(entry["user_message"], 0), anyText(entry["prompt"], 0), anyText(entry["input"], 0), anyText(entry["content"], 0), anyText(entry, 0))
			text := normalizeAntigravityUser(raw)
			if text != "" {
				current = &turn{UserMessages: []string{truncate(text, 2000)}, UserStepIndex: step, AssistantStepIndex: step}
			} else {
				current = nil
			}
			continue
		}
		assistant := ""
		if stringValue(entry["source"]) == "MODEL" && stringValue(entry["type"]) == "PLANNER_RESPONSE" {
			assistant = stringValue(entry["content"])
		} else if role == "assistant" {
			assistant = anyText(entry, 0)
		}
		if current != nil && assistant != "" {
			current.AssistantMessages = []string{truncate(assistant, 2000)}
			current.LastAssistant = truncate(assistant, 2000)
			current.AssistantStepIndex = step
		}
	}
	flush()
	return values
}

func antigravityRole(entry map[string]any) string {
	candidates := []string{stringValue(entry["role"]), stringValue(entry["type"]), stringValue(mapValue(entry["author"])["role"]), stringValue(mapValue(entry["message"])["role"]), stringValue(entry["sender"]), stringValue(entry["source"])}
	value := strings.ToLower(strings.Join(candidates, " "))
	if strings.Contains(value, "user") || strings.Contains(value, "human") {
		return "user"
	}
	if strings.Contains(value, "assistant") || strings.Contains(value, "agent") || strings.Contains(value, "model") {
		return "assistant"
	}
	return ""
}

func normalizeAntigravityUser(value string) string {
	value = strings.TrimSpace(value)
	if match := userRequestPattern.FindStringSubmatch(value); len(match) > 1 {
		return compactWhitespace(match[1], 2000)
	}
	return strings.TrimSpace(additionalMetadataPattern.ReplaceAllString(value, ""))
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if item := stringValue(value[key]); item != "" {
			return item
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func nonEmptySlice(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []string{strings.TrimSpace(value)}
}

func userHome() string {
	if value := strings.TrimSpace(os.Getenv("HOME")); value != "" {
		return value
	}
	value, _ := os.UserHomeDir()
	return value
}

var _ = json.Marshal
