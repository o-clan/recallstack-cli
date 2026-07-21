package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

func FormatRetrieve(response map[string]any, now time.Time) string {
	strategy := object(response["strategy"])
	meta := object(response["meta"])
	packing := object(strategy["evidence_packing"])
	graph := object(strategy["graph_expansion"])
	lines := []string{"STRATEGY", fmt.Sprintf("%s · route=%s · mode=%s · packing=%s · graph_depth=%v", textOr(strategy["family"], "hybrid_graph_augmented"), textOr(meta["intent_route"], "unknown"), textOr(meta["retrieval_mode"], "standard"), textOr(packing["objective"], "balanced"), firstValue(graph["depth"], meta["graph_depth"], 0))}
	if query := strings.TrimSpace(text(strategy["normalized_query"])); query != "" {
		lines = append(lines, "Normalized query: "+query)
	}
	evidence := object(response["evidence"])
	context := objects(evidence["context"], 3)
	primary := objects(evidence["primary"], 5)
	if len(context) > 0 {
		lines = append(lines, "", "CONTEXT")
		for _, item := range context {
			lines = appendEvidence(lines, item, now)
		}
	}
	lines = append(lines, "", "EVIDENCE")
	if len(primary) == 0 {
		lines = append(lines, "No primary evidence returned.")
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}
	for _, item := range primary {
		lines = appendEvidence(lines, item, now)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func FormatQuery(result QueryResult, options QueryOptions, now time.Time) string {
	if options.JSON {
		payload := copyMap(result.Response)
		if result.Synthesis != nil {
			payload["synthesis"] = result.Synthesis
		}
		return PrettyJSON(payload)
	}
	if result.Synthesis != nil {
		if options.SynthesizeOnly {
			return result.Synthesis.Answer
		}
		lines := []string{"SYNTHESIS", result.Synthesis.Answer}
		if len(result.Synthesis.Caveats) > 0 {
			lines = append(lines, "", "CAVEATS")
			for _, caveat := range result.Synthesis.Caveats {
				lines = append(lines, "- "+caveat)
			}
		}
		if !options.SynthesizeWithCaveats {
			lines = append(lines, "", FormatRetrieve(result.Response, now))
		}
		return strings.Join(lines, "\n")
	}
	if structured, ok := result.Response["structured"].(string); ok {
		return structured
	}
	return PrettyJSON(result.Response)
}

func PrettyJSON(value any) string { data, _ := jsonMarshalIndent(value); return string(data) }

// FormatStructured applies the same terminal emphasis as the TypeScript CLI.
// Passing color=false preserves the text byte-for-byte.
func FormatStructured(value string, color bool) string {
	if !color {
		return value
	}
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		switch line {
		case "SYNTHESIS", "STRATEGY", "CONTEXT", "EVIDENCE", "CAVEATS":
			lines[i] = "\x1b[96m\x1b[1m" + line + "\x1b[0m"
		default:
			if score := evidenceScore.FindStringSubmatch(line); score != nil {
				lines[i] = "\x1b[96m\x1b[1m" + score[1] + "\x1b[0m" + score[2] + "\x1b[1m" + score[3] + "\x1b[0m"
			} else if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "- ") {
				lines[i] = "\x1b[2m" + line + "\x1b[0m"
			}
		}
	}
	return strings.Join(lines, "\n")
}

var (
	sentence      = regexp.MustCompile(`^.+?[.!?](?:\s|$)`)
	evidenceScore = regexp.MustCompile(`^(\d+\.\d{2})(\s+)(.+)$`)
)

func splitContent(value string) (string, string) {
	normalized := strings.Join(strings.Fields(value), " ")
	if normalized == "" {
		return "Untitled memory item", ""
	}
	match := sentence.FindString(normalized)
	title := normalized
	consumed := len(title)
	if match != "" {
		title = match
		consumed = len(match)
	}
	title = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(title), "."), "!"), "?")
	return title, strings.TrimSpace(normalized[consumed:])
}
func appendEvidence(lines []string, item map[string]any, now time.Time) []string {
	title, detail := splitContent(text(item["content"]))
	confidence := "--"
	switch value := item["confidence"].(type) {
	case float64:
		confidence = fmt.Sprintf("%.2f", value)
	case float32:
		confidence = fmt.Sprintf("%.2f", value)
	case int:
		confidence = fmt.Sprintf("%.2f", float64(value))
	}
	id := textOr(item["id"], "unknown")
	lines = append(lines, fmt.Sprintf("%s  [%s] %s", confidence, id, title))
	if detail != "" {
		lines = append(lines, "    "+detail)
	}
	project := object(item["project"])
	metadata := []string{}
	if value := text(project["slug"]); value != "" {
		metadata = append(metadata, value)
	} else if value := text(project["name"]); value != "" {
		metadata = append(metadata, value)
	}
	metadata = append(metadata, relativeTime(item["createdAt"], now))
	lines = append(lines, "    "+strings.Join(metadata, " · "), "")
	return lines
}
func relativeTime(value any, now time.Time) string {
	raw, ok := value.(string)
	if !ok {
		return "unknown time"
	}
	created, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "unknown time"
	}
	minutes := int(now.Sub(created).Minutes() + 0.5)
	if minutes < 0 {
		minutes = 0
	}
	if minutes < 1 {
		return "just now"
	}
	if minutes < 60 {
		return fmt.Sprintf("%d min ago", minutes)
	}
	hours := int(float64(minutes)/60 + 0.5)
	if hours < 24 {
		suffix := "s"
		if hours == 1 {
			suffix = ""
		}
		return fmt.Sprintf("%d hour%s ago", hours, suffix)
	}
	days := int(float64(hours)/24 + 0.5)
	suffix := "s"
	if days == 1 {
		suffix = ""
	}
	return fmt.Sprintf("%d day%s ago", days, suffix)
}
func object(value any) map[string]any {
	out, _ := value.(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	return out
}
func objects(value any, limit int) []map[string]any {
	raw, _ := value.([]any)
	out := []map[string]any{}
	for _, item := range raw {
		if value, ok := item.(map[string]any); ok {
			out = append(out, value)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}
func text(value any) string { out, _ := value.(string); return out }
func textOr(value any, fallback string) string {
	if out := text(value); out != "" {
		return out
	}
	return fallback
}
func firstValue(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
