package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/identity"
	"github.com/o-clan/recallstack-cli/internal/project"
	"github.com/o-clan/recallstack-cli/internal/state"
)

// thoughtContext is who is acting (agent session or by hand) and where.
type thoughtContext struct {
	id      identity.Identity
	folder  project.Folder
	project project.Entry
	known   bool
}

func (r *runtimeServices) thoughtContext(ctx context.Context) thoughtContext {
	id := identity.Find(r.cwd)
	folder := project.Detect(r.cwd)
	entry, known := project.Lookup(folder.Key)
	if !known && id.Session.ProjectID != "" {
		entry, known = project.Entry{ProjectID: id.Session.ProjectID, Name: id.Session.ProjectName}, true
	}
	if !known && folder.InRepo && r.svc.SignedIn() {
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resolved, _, err := project.Resolve(resolveCtx, r.svc, folder)
		cancel()
		if err == nil && resolved.ProjectID != "" {
			entry, known = resolved, true
		}
	}
	return thoughtContext{id: id, folder: folder, project: entry, known: known}
}

func (t thoughtContext) agent() string {
	if t.id.ByHand() {
		return "hand"
	}
	return t.id.Agent
}

func (t thoughtContext) sessionID() any {
	if t.id.ByHand() {
		return nil
	}
	return t.id.SessionID
}

// changesKey is the per-session undo stack file key ("" = by hand).
func (t thoughtContext) changesKey() string {
	if t.id.ByHand() {
		return ""
	}
	return t.id.SessionID
}

func (t thoughtContext) projectID() any {
	if t.known && t.project.ProjectID != "" {
		return t.project.ProjectID
	}
	return nil
}

func (t thoughtContext) projectName() string {
	if t.known && t.project.Name != "" {
		return t.project.Name
	}
	if t.folder.InRepo {
		return t.folder.Name
	}
	return "this folder"
}

// uuid7 is a client-generated change id (UUID v7).
func uuid7() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	millis := uint64(time.Now().UnixMilli())
	for index := 0; index < 6; index++ {
		value[index] = byte(millis >> (40 - 8*index))
	}
	value[6] = (value[6] & 0x0f) | 0x70
	value[8] = (value[8] & 0x3f) | 0x80
	text := hex.EncodeToString(value[:])
	return text[0:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

func randomHex(n int) string {
	value := make([]byte, n)
	_, _ = rand.Read(value)
	return hex.EncodeToString(value)
}

func sha1Text(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, ":")))
	return hex.EncodeToString(sum[:])
}

var refPattern = regexp.MustCompile(`^(?:([a-z0-9][a-z0-9-]{0,62}|me):)?([GFDQTCH])([1-9][0-9]{0,5})$`)

func isRef(value string) bool { return refPattern.MatchString(strings.TrimSpace(value)) }

// parseWhere turns --where into the contract's scope object.
func parseWhere(value string) (map[string]any, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return nil, nil
	case value == "everywhere":
		return map[string]any{"scope": "everywhere"}, nil
	case value == "project":
		return map[string]any{"scope": "project"}, nil
	case strings.HasPrefix(value, "project:") && strings.TrimSpace(strings.TrimPrefix(value, "project:")) != "":
		return map[string]any{"scope": "project", "project": strings.TrimSpace(strings.TrimPrefix(value, "project:"))}, nil
	case strings.HasPrefix(value, "path:") && strings.TrimSpace(strings.TrimPrefix(value, "path:")) != "":
		path := strings.Trim(strings.TrimSpace(strings.TrimPrefix(value, "path:")), "/")
		return map[string]any{"scope": "path", "path": path}, nil
	}
	return nil, fmt.Errorf("--where takes everywhere, project, project:<name> or path:<path>")
}

func whereDisplay(where map[string]any, projectName string) string {
	switch where["scope"] {
	case "everywhere":
		return "everywhere"
	case "path":
		return fmt.Sprint(where["path"]) + " in " + projectName
	}
	if name, ok := where["project"].(string); ok && name != "" {
		return name + " only"
	}
	return projectName + " only"
}

// firstSentence is the "{first sentence of what}" in result lines.
func firstSentence(what string) string {
	what = strings.TrimSpace(what)
	for index, r := range what {
		if (r == '.' || r == '?' || r == '!') && (index+1 == len(what) || what[index+1] == ' ') {
			return what[:index+1]
		}
	}
	return what
}

// sentence makes "{x}." without a doubled full stop.
func sentence(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if strings.HasSuffix(value, ".") || strings.HasSuffix(value, "?") || strings.HasSuffix(value, "!") {
		return value
	}
	return value + "."
}

func cutQuote(quote string) string {
	runes := []rune(strings.TrimSpace(quote))
	if len(runes) <= 120 {
		return string(runes)
	}
	cut := 120
	for index := 120; index > 100; index-- {
		if runes[index] == ' ' {
			cut = index
			break
		}
	}
	return strings.TrimSpace(string(runes[:cut])) + "…"
}

func dateLabel(value string, withYear bool) string {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		if at, err = time.Parse("2006-01-02", value); err != nil {
			return value
		}
	}
	at = at.Local()
	if withYear || at.Year() != time.Now().Year() {
		return at.Format("Jan 2, 2006")
	}
	return at.Format("Jan 2")
}

var kindColour = map[string]string{"goal": "38;5;78", "focus": "38;5;75", "decision": "38;5;221", "question": "38;5;177", "taste": "38;5;211"}

// dot is the kind's colour dot, shown only to people (a terminal).
func (r *runtimeServices) dot(kind string, inForce bool) string {
	file, ok := r.deps.Stdout.(*os.File)
	if !ok {
		return ""
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 || os.Getenv("NO_COLOR") != "" {
		return ""
	}
	glyph := "●"
	if !inForce {
		glyph = "○"
	}
	colour := kindColour[kind]
	if colour == "" {
		return glyph + " "
	}
	return "\x1b[" + colour + "m" + glyph + "\x1b[0m "
}

// recordSummary is the contract's RecordSummary (plus the fields Record adds).
type recordSummary struct {
	ID           string `json:"id"`
	Ref          string `json:"ref"`
	QualifiedRef string `json:"qualified_ref"`
	Kind         string `json:"kind"`
	What         string `json:"what"`
	State        string `json:"state"`
	Certainty    string `json:"certainty"`
	Imported     bool   `json:"imported"`
	Scope        struct {
		Scope       string `json:"scope"`
		ProjectID   string `json:"project_id"`
		ProjectName string `json:"project_name"`
		Path        string `json:"path"`
		Display     string `json:"display"`
	} `json:"scope"`
	Why        *string  `json:"why"`
	WhyIsQuote bool     `json:"why_is_quote"`
	InsteadOf  []string `json:"instead_of"`
	Answers    *struct {
		Ref      *string `json:"ref"`
		Question string  `json:"question"`
	} `json:"answers"`
	Source *struct {
		Agent        string  `json:"agent"`
		AgentName    string  `json:"agent_name"`
		Kind         string  `json:"kind"`
		SetAt        string  `json:"set_at"`
		Quote        *string `json:"quote"`
		ImportedFrom *struct {
			Path      string `json:"path"`
			LineStart int    `json:"line_start"`
		} `json:"imported_from"`
	} `json:"source"`
	SetAt          string `json:"set_at"`
	StateChangedAt string `json:"state_changed_at"`
	AdjustedCount  int    `json:"adjusted_count"`
	Review         string `json:"review"`
	URL            string `json:"url"`
}

func decodeInto(raw json.RawMessage, target any) {
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, target)
	}
}

// briefLines finds a ref's lines in the cached brief: the "{ref} {what}" line
// and its "   why:" sub-line. Used offline.
func briefLines(projectID, ref string) (string, string) {
	text, _, ok := state.LoadBriefCache(projectID)
	if !ok {
		return "", ""
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ref+" ") {
			what := strings.TrimSpace(strings.TrimPrefix(trimmed, ref+" "))
			why := ""
			if index+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index+1]), "why: ") {
				why = strings.TrimPrefix(strings.TrimSpace(lines[index+1]), "why: ")
			}
			return what, why
		}
	}
	return "", ""
}
