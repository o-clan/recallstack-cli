package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/hooks"
	"github.com/o-clan/recallstack-cli/internal/pending"
	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/secrets"
	"github.com/o-clan/recallstack-cli/internal/state"
)

type recordLink struct {
	Type      string        `json:"type"`
	Direction string        `json:"direction"`
	Record    recordSummary `json:"record"`
	Hold      *struct {
		Ref     string        `json:"ref"`
		Against recordSummary `json:"against"`
	} `json:"hold"`
	At    string `json:"at"`
	Quote string `json:"quote"`
}

type recordDetail struct {
	Record  recordSummary `json:"record"`
	Links   []recordLink  `json:"links"`
	Dropped *struct {
		At    string `json:"at"`
		Agent string `json:"agent"`
		Quote string `json:"quote"`
	} `json:"dropped"`
}

func (r *runtimeServices) projectIDByName(ctx context.Context, name string) (string, string, error) {
	var response struct {
		Projects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"projects"`
	}
	if err := r.svc.Do(ctx, remote.Request{Method: http.MethodGet, Path: "/v1/projects"}, &response); err != nil {
		return "", "", err
	}
	for _, item := range response.Projects {
		if strings.EqualFold(item.Name, name) || strings.EqualFold(item.Slug, name) {
			return item.ID, item.Name, nil
		}
	}
	return "", "", &ExitError{Code: 1, Message: fmt.Sprintf("You have no project called %s.", name)}
}

func (r *runtimeServices) runWhy(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"project"}, []string{"json"}))
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(parsed.positionals, " "))
	if query == "" {
		return fmt.Errorf("usage: %s", usage["why"])
	}
	if !r.svc.SignedIn() {
		return &ExitError{Code: 1, Message: "Sign in first:  recallstack login"}
	}
	ctx := context.Background()
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tc := r.thoughtContext(ctx)
	projectID, projectName := "", tc.projectName()
	if id, ok := tc.projectID().(string); ok {
		projectID = id
	}
	if name := strings.TrimSpace(parsed.values["project"]); name != "" {
		projectID, projectName, err = r.projectIDByName(requestCtx, name)
		if err != nil {
			return r.networkErrorOr(err)
		}
	}
	params := url.Values{}
	if projectID != "" {
		params.Set("project_id", projectID)
	}
	out := r.deps.Stdout
	if isRef(query) {
		params.Set("history", "1")
		var raw json.RawMessage
		err := r.svc.Do(requestCtx, remote.Request{Method: http.MethodGet, Path: "/v1/records/" + url.PathEscape(query) + "?" + params.Encode()}, &raw)
		if api.Code(err) == "REF_NOT_FOUND" || api.Status(err) == 404 {
			return &ExitError{Code: 1, Message: fmt.Sprintf("No %s in %s or your taste. Find it by meaning:  recallstack why \"<words>\"", query, projectName)}
		}
		if err != nil {
			return r.networkError(err)
		}
		if parsed.bools["json"] {
			_, err := fmt.Fprintln(out, string(raw))
			return err
		}
		var detail recordDetail
		decodeInto(raw, &detail)
		r.printWhy(detail, true)
		return nil
	}
	params.Set("q", secrets.StripString(query))
	var raw json.RawMessage
	err = r.svc.Do(requestCtx, remote.Request{Method: http.MethodGet, Path: "/v1/why?" + params.Encode()}, &raw)
	if api.Code(err) == "SEARCH_UNAVAILABLE" {
		return &ExitError{Code: 1, Message: "Search isn't available right now. Look a thought up by its ref instead:  recallstack why D1"}
	}
	if err != nil {
		return r.networkError(err)
	}
	if parsed.bools["json"] {
		_, err := fmt.Fprintln(out, string(raw))
		return err
	}
	var response struct {
		Threshold float64 `json:"threshold"`
		Matches   []struct {
			Ref    string          `json:"ref"`
			Score  float64         `json:"score"`
			Record json.RawMessage `json:"record"`
		} `json:"matches"`
	}
	decodeInto(raw, &response)
	if len(response.Matches) == 0 || response.Matches[0].Score < response.Threshold {
		fmt.Fprintf(out, "Nothing recorded about \"%s\" in %s or everywhere, including dropped and replaced thoughts.\n", query, projectName)
		return nil
	}
	var first recordDetail
	decodeInto(response.Matches[0].Record, &first.Record)
	var withLinks struct {
		Links   []recordLink `json:"links"`
		Dropped *struct {
			At    string `json:"at"`
			Agent string `json:"agent"`
			Quote string `json:"quote"`
		} `json:"dropped"`
	}
	decodeInto(response.Matches[0].Record, &withLinks)
	first.Links, first.Dropped = withLinks.Links, withLinks.Dropped
	r.printWhy(first, false)
	close := []string{}
	for _, match := range response.Matches[1:] {
		if match.Score < response.Threshold || len(close) == 2 {
			continue
		}
		var summary recordSummary
		decodeInto(match.Record, &summary)
		close = append(close, fmt.Sprintf("   %s %s", firstNonEmptyString(summary.Ref, match.Ref), summary.What))
	}
	if len(close) > 0 {
		fmt.Fprintln(out, "\nAlso close:")
		for _, line := range close {
			fmt.Fprintln(out, line)
		}
	}
	return nil
}

func (r *runtimeServices) networkErrorOr(err error) error {
	if _, ok := err.(*ExitError); ok {
		return err
	}
	return r.networkError(err)
}

var stateLabels = map[string]string{"dropped": "dropped", "replaced": "replaced", "set_aside": "set aside", "answered": "answered", "retracted": "removed", "merged": "merged"}

// byRef is true for why <ref>; a search by words ends with the ref's own hint.
func (r *runtimeServices) printWhy(detail recordDetail, byRef bool) {
	out := r.deps.Stdout
	record := detail.Record
	inForce := record.State == "in_force" || record.State == ""
	head := fmt.Sprintf("%s%s %s", r.dot(record.Kind, inForce), record.Ref, record.What)
	if label := stateLabels[record.State]; label != "" {
		head += "  " + label
	}
	fmt.Fprintln(out, head)
	switch {
	case record.Why != nil && *record.Why != "" && record.WhyIsQuote:
		fmt.Fprintf(out, "   why: \"%s\"\n", *record.Why)
	case record.Why != nil && *record.Why != "":
		fmt.Fprintf(out, "   why: %s\n", *record.Why)
	default:
		fmt.Fprintln(out, "   why: not recorded")
	}
	if len(record.InsteadOf) > 0 {
		fmt.Fprintf(out, "   instead of: %s\n", strings.Join(record.InsteadOf, ", "))
	}
	if source := record.Source; source != nil {
		setAt := firstNonEmptyString(source.SetAt, record.SetAt)
		if source.ImportedFrom != nil && source.ImportedFrom.Path != "" {
			fmt.Fprintf(out, "   imported from %s · %s\n", source.ImportedFrom.Path, dateLabel(setAt, true))
		} else {
			parts := []string{firstNonEmptyString(source.AgentName, agentDisplay(source.Agent))}
			if setAt != "" {
				parts = append(parts, dateLabel(setAt, true))
			}
			if source.Quote != nil && *source.Quote != "" {
				parts = append(parts, "\""+cutQuote(*source.Quote)+"\"")
			}
			fmt.Fprintf(out, "   %s\n", strings.Join(parts, " · "))
		}
	}
	for _, link := range detail.Links {
		switch {
		case link.Type == "answers":
			fmt.Fprintf(out, "   answers %s \"%s\"\n", link.Record.Ref, link.Record.What)
		case link.Type == "came_up_during_hold" && link.Hold != nil:
			fmt.Fprintf(out, "   came up while holding on %s \"%s\"\n", link.Hold.Against.Ref, link.Hold.Against.What)
		case link.Type == "replaces" && link.Direction == "in":
			fmt.Fprintf(out, "   replaced by %s %s (%s)\n", link.Record.Ref, link.Record.What, dateLabel(link.At, false))
		case link.Type == "replaces":
			fmt.Fprintf(out, "   replaced \"%s\" (%s)\n", link.Record.What, dateLabel(link.At, false))
		case link.Type == "set_aside_for" && link.Direction == "in":
			fmt.Fprintf(out, "   kept over %s %s (%s)\n", link.Record.Ref, link.Record.What, dateLabel(link.At, false))
		case link.Type == "set_aside_for" && link.Quote != "":
			fmt.Fprintf(out, "   set aside for %s: \"%s\"\n", link.Record.Ref, cutQuote(link.Quote))
		case link.Type == "set_aside_for":
			fmt.Fprintf(out, "   set aside for %s (%s)\n", link.Record.Ref, dateLabel(link.At, false))
		}
	}
	if record.Answers != nil && record.Answers.Question != "" && !hasLink(detail.Links, "answers") {
		if record.Answers.Ref != nil && *record.Answers.Ref != "" {
			fmt.Fprintf(out, "   answers %s \"%s\"\n", *record.Answers.Ref, record.Answers.Question)
		} else {
			fmt.Fprintf(out, "   answers \"%s\"\n", record.Answers.Question)
		}
	}
	since := dateLabel(firstNonEmptyString(record.SetAt, record.StateChangedAt), true)
	switch {
	case inForce && record.AdjustedCount == 0:
		fmt.Fprintf(out, "   in force since %s, never changed\n", since)
	case inForce && record.AdjustedCount == 1:
		fmt.Fprintf(out, "   in force since %s, adjusted once\n", since)
	case inForce:
		fmt.Fprintf(out, "   in force since %s, adjusted %d times\n", since, record.AdjustedCount)
	case record.State == "dropped":
		line := "   dropped"
		if detail.Dropped != nil {
			line += " " + dateLabel(detail.Dropped.At, true) + " in " + agentDisplay(detail.Dropped.Agent)
			if detail.Dropped.Quote != "" {
				line += " · \"" + cutQuote(detail.Dropped.Quote) + "\""
			}
		} else if record.StateChangedAt != "" {
			line += " " + dateLabel(record.StateChangedAt, true)
		}
		fmt.Fprintln(out, line)
		fmt.Fprintf(out, "   left out of briefs since then · bring it back:  recallstack undo %s\n", record.Ref)
	}
	// The Thoughts app isn't out yet, so no link: point at the commands instead.
	waiting := inForce && (record.Review == "pending" || ((record.Certainty == "agent_proposed" || record.Certainty == "inferred") && record.Review != "confirmed"))
	switch {
	case waiting:
		fmt.Fprintln(out, "   not confirmed yet, so it's left out of briefs · settle it:  recallstack review")
	case !byRef && record.Ref != "":
		fmt.Fprintf(out, "   recallstack why %s\n", record.Ref)
	}
}

func hasLink(links []recordLink, kind string) bool {
	for _, link := range links {
		if link.Type == kind {
			return true
		}
	}
	return false
}

func (r *runtimeServices) runAdjust(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"where", "text", "why", "quote"}, []string{"json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 {
		return fmt.Errorf("usage: %s", usage["adjust"])
	}
	ref := strings.TrimSpace(parsed.positionals[0])
	where, err := parseWhere(parsed.values["where"])
	if err != nil {
		return err
	}
	text := secrets.StripString(strings.TrimSpace(parsed.values["text"]))
	why := secrets.StripString(strings.TrimSpace(parsed.values["why"]))
	quote := secrets.StripString(strings.TrimSpace(parsed.values["quote"]))
	if where == nil && text == "" && why == "" {
		return fmt.Errorf("say what changes: --where, --text or --why")
	}
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	if err := r.checkCorrection(tc, ref, quote); err != nil {
		return err
	}
	changeID := uuid7()
	body := map[string]any{"change_id": changeID, "project_id": tc.projectID(), "session_id": tc.sessionID(), "agent": tc.agent(), "where": where, "what": nullable(text), "why": nullable(why), "quote": nullable(quote)}
	var response struct {
		ChangeID string        `json:"change_id"`
		Record   recordSummary `json:"record"`
		Previous struct {
			What  string `json:"what"`
			Why   string `json:"why"`
			Scope struct {
				Display string `json:"display"`
			} `json:"scope"`
		} `json:"previous"`
		Detail         string          `json:"detail"`
		ImportedSource *importedSource `json:"imported_source"`
	}
	queued, err := r.sendChange(ctx, "/v1/records/"+url.PathEscape(ref)+"/adjust", body, changeID, "adjust", &response)
	if err != nil {
		return r.thoughtError(err, tc, ref)
	}
	change := state.Change{ChangeID: changeID, Kind: "adjust", Ref: ref, What: text}
	if queued {
		change.PendingID = r.pendingID(changeID)
	}
	_ = state.AddChange(tc.changesKey(), change)
	out := r.deps.Stdout
	if queued {
		switch {
		case where != nil:
			fmt.Fprintf(out, "Adjusted %s (will sync). Now %s.\n", ref, whereDisplay(where, tc.projectName()))
		case text != "":
			fmt.Fprintf(out, "Adjusted %s (will sync) · now \"%s\".\n", ref, text)
		default:
			fmt.Fprintf(out, "Adjusted %s (will sync). New reason: %s\n", ref, sentence(why))
		}
		return nil
	}
	if parsed.bools["json"] {
		return writeJSON(out, response)
	}
	record := response.Record
	dot := r.dot(record.Kind, true)
	switch {
	case where != nil:
		fmt.Fprintf(out, "%sAdjusted %s · %s Now %s, was %s.\n", dot, record.Ref, firstSentence(record.What), record.Scope.Display, response.Previous.Scope.Display)
	case text != "":
		fmt.Fprintf(out, "%sAdjusted %s · now \"%s\", was \"%s\".\n", dot, record.Ref, record.What, response.Previous.What)
	default:
		fmt.Fprintf(out, "%sAdjusted %s · %s New reason: %s\n", dot, record.Ref, firstSentence(record.What), sentence(why))
	}
	r.printImportedSource(response.ImportedSource)
	return nil
}

type importedSource struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

func (r *runtimeServices) printImportedSource(source *importedSource) {
	if source == nil || source.Path == "" {
		return
	}
	fmt.Fprintf(r.deps.Stdout, "%s line %d still says this, and agents read that file directly.\n", source.Path, source.LineStart)
}

// checkCorrection applies the shared rules for adjust and drop.
func (r *runtimeServices) checkCorrection(tc thoughtContext, ref, quote string) error {
	if !isRef(ref) {
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s isn't a ref. Refs look like D6, T2 or recallstack:D6.", ref)}
	}
	bare := strings.TrimPrefix(ref, refNamespace(ref))
	if strings.HasPrefix(bare, "C") || strings.HasPrefix(bare, "H") {
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s is waiting on you. Settle it with recallstack resolve %s <choice>.", ref, ref)}
	}
	if quote == "" && !tc.id.ByHand() {
		return &ExitError{Code: 1, Message: "Pass the person's words with --quote \"<their words>\". Agents change a thought only when the person says so."}
	}
	return nil
}

func (r *runtimeServices) sendChange(ctx context.Context, path string, body map[string]any, changeID, label string, out any) (bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.svc.Send(requestCtx, remote.Write{Path: path, Body: body, Label: label, ChangeID: changeID, IdempotencyKey: changeID, Origin: label}, out)
}

func (r *runtimeServices) pendingID(changeID string) string {
	if record, ok := r.svc.Queue().FindByChangeID(changeID); ok {
		return record.ID
	}
	return ""
}

func (r *runtimeServices) runDrop(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"quote"}, []string{"json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 {
		return fmt.Errorf("usage: %s", usage["drop"])
	}
	ref := strings.TrimSpace(parsed.positionals[0])
	quote := secrets.StripString(strings.TrimSpace(parsed.values["quote"]))
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	if err := r.checkCorrection(tc, ref, quote); err != nil {
		return err
	}
	changeID := uuid7()
	var response struct {
		ChangeID        string          `json:"change_id"`
		Record          recordSummary   `json:"record"`
		EndedExceptions int             `json:"ended_exceptions"`
		ImportedSource  *importedSource `json:"imported_source"`
	}
	queued, err := r.sendChange(ctx, "/v1/records/"+url.PathEscape(ref)+"/drop", map[string]any{
		"change_id": changeID, "project_id": tc.projectID(), "session_id": tc.sessionID(), "agent": tc.agent(), "quote": nullable(quote),
	}, changeID, "drop", &response)
	if err != nil {
		return r.thoughtError(err, tc, ref)
	}
	change := state.Change{ChangeID: changeID, Kind: "drop", Ref: ref, What: response.Record.What}
	out := r.deps.Stdout
	if queued {
		change.PendingID = r.pendingID(changeID)
		_ = state.AddChange(tc.changesKey(), change)
		fmt.Fprintf(out, "Dropped %s (will sync). Kept in history, left out of briefs.\n", ref)
		return nil
	}
	_ = state.AddChange(tc.changesKey(), change)
	if parsed.bools["json"] {
		return writeJSON(out, response)
	}
	fmt.Fprintf(out, "%sDropped %s · %s Kept in history, left out of briefs.\n", r.dot(response.Record.Kind, false), response.Record.Ref, sentence(firstSentence(response.Record.What)))
	r.printImportedSource(response.ImportedSource)
	return nil
}

var undoVerbs = map[string]string{"drop": "dropping", "adjust": "adjusting", "note": "noting", "capture": "noting", "replace": "replacing", "exception": "the exception on", "conflict_settle": "settling", "confirm": "confirming", "undo": "undoing"}

func (r *runtimeServices) runUndo(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) > 1 {
		return fmt.Errorf("usage: %s", usage["undo"])
	}
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	out := r.deps.Stdout
	undoID := uuid7()
	body := map[string]any{"change_id": undoID, "project_id": tc.projectID(), "session_id": tc.sessionID(), "agent": tc.agent()}
	var response struct {
		ChangeID string `json:"change_id"`
		Undone   struct {
			ChangeID    string  `json:"change_id"`
			Kind        string  `json:"kind"`
			Ref         string  `json:"ref"`
			What        string  `json:"what"`
			Result      string  `json:"result"`
			OldScope    *string `json:"old_scope"`
			RestoredRef *string `json:"restored_ref"`
			Count       *int    `json:"count"`
		} `json:"undone"`
	}
	var target *state.Change
	path := ""
	if len(parsed.positionals) == 1 {
		ref := strings.TrimSpace(parsed.positionals[0])
		if !isRef(ref) {
			return &ExitError{Code: 1, Message: fmt.Sprintf("%s isn't a ref. Refs look like D6, T2 or recallstack:D6.", ref)}
		}
		if bare := strings.TrimPrefix(ref, refNamespace(ref)); strings.HasPrefix(bare, "H") {
			return &ExitError{Code: 1, Message: fmt.Sprintf("Holds aren't undone; settle it with recallstack resolve %s <choice>.", ref)}
		}
		path = "/v1/records/" + url.PathEscape(ref) + "/undo"
	} else {
		changes := state.LoadChanges(tc.changesKey())
		for index := len(changes) - 1; index >= 0; index-- {
			if !changes[index].Undone {
				target = &changes[index]
				break
			}
		}
		if target == nil {
			fmt.Fprintln(out, "Nothing to undo from this session. To undo a change to a thought, name it:  recallstack undo <ref>")
			return nil
		}
		// A change still waiting in the queue is simply taken out of it.
		if target.PendingID != "" {
			if removed, _ := r.svc.Queue().RemoveByID(target.PendingID); removed {
				_ = state.UpdateChange(tc.changesKey(), target.ChangeID, func(c *state.Change) { c.Undone = true })
				subject := undoVerbs[target.Kind]
				if target.Ref != "" {
					subject += " " + target.Ref
				}
				fmt.Fprintf(out, "Undid %s · %s Removed before it was sent.\n", subject, sentence(firstSentence(target.What)))
				return nil
			}
		}
		path = "/v1/changes/" + url.PathEscape(target.ChangeID) + "/undo"
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = r.svc.Do(requestCtx, remote.Request{Method: http.MethodPost, Path: path, Body: body, IdempotencyKey: "undo-" + strings.TrimPrefix(path, "/v1/") + "-" + randomHex(4)}, &response)
	switch api.Code(err) {
	case "UNDO_STALE":
		details := api.Details(err)
		ref := fmt.Sprint(details["ref"])
		if target != nil && target.Ref != "" {
			ref = target.Ref
		}
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s changed after that. Undo that first:  recallstack undo %s", ref, ref)}
	case "NOTHING_TO_UNDO":
		if target != nil {
			// Undone some other way (by ref, or in the app): skip it and undo
			// the one before, which is what "undo" means here.
			_ = state.UpdateChange(tc.changesKey(), target.ChangeID, func(c *state.Change) { c.Undone = true })
			return r.runUndo(args)
		}
		fmt.Fprintln(out, "That change was already undone.")
		return nil
	}
	if err != nil {
		if api.Retryable(err) || pending.IsAuthRequired(err) {
			return &ExitError{Code: 1, Message: fmt.Sprintf("Can't reach %s, so nothing was undone. Try again when you're back online.", r.apiHostLabel())}
		}
		return r.thoughtError(err, tc, firstNonEmptyString(strings.Join(parsed.positionals, ""), "that"))
	}
	if target != nil {
		_ = state.UpdateChange(tc.changesKey(), target.ChangeID, func(c *state.Change) { c.Undone = true })
	} else if response.Undone.ChangeID != "" {
		// undo <ref> reversed one of this session's own changes: plain undo skips it later.
		_ = state.UpdateChange(tc.changesKey(), response.Undone.ChangeID, func(c *state.Change) { c.Undone = true })
	}
	if parsed.bools["json"] {
		return writeJSON(out, response)
	}
	undone := response.Undone
	if undone.Kind == "import" || undone.Result == "import_removed" {
		count := 0
		if undone.Count != nil {
			count = *undone.Count
		}
		fmt.Fprintf(out, "Undid the import · %d thoughts removed.\n", count)
		return nil
	}
	if undone.Kind == "confirm" && undone.Count != nil && *undone.Count > 1 {
		fmt.Fprintf(out, "Undid confirming %d thoughts. Back in review:  recallstack review\n", *undone.Count)
		return nil
	}
	tail := ""
	switch undone.Result {
	case "back_in_force":
		tail = "Back in force."
	case "back_to_scope":
		if undone.OldScope != nil {
			tail = "Back to " + *undone.OldScope + "."
		} else {
			tail = "Back as it was."
		}
	case "removed":
		tail = "Removed."
	case "restored":
		if undone.RestoredRef != nil {
			tail = *undone.RestoredRef + " back in force."
		}
	case "reapplied":
		tail = "Reapplied."
	case "back_in_review":
		tail = "Back in review:  recallstack review"
	}
	verb := undoVerbs[undone.Kind]
	if verb == "" {
		verb = undone.Kind
	}
	fmt.Fprintf(out, "Undid %s %s · %s %s\n", verb, undone.Ref, sentence(firstSentence(undone.What)), tail)
	return nil
}

func (r *runtimeServices) runBrief(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"agent"}, []string{"all", "json"}))
	if err != nil {
		return err
	}
	agent := strings.TrimSpace(parsed.values["agent"])
	if agent == "" {
		agent = "claude"
	}
	if hooks.NormalizeAgent(agent) == "" {
		return fmt.Errorf("--agent is one of claude, codex, cursor, copilot or antigravity")
	}
	agent = hooks.NormalizeAgent(agent)
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	projectID := ""
	if id, ok := tc.projectID().(string); ok {
		projectID = id
	}
	out := r.deps.Stdout
	if !r.svc.SignedIn() {
		return &ExitError{Code: 1, Message: "Sign in first, so the brief can load:  recallstack login"}
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	params := url.Values{}
	if projectID != "" {
		params.Set("project_id", projectID)
	}
	params.Set("agent", agent)
	if parsed.bools["all"] {
		params.Set("all", "1")
	}
	var raw json.RawMessage
	err = r.svc.Do(requestCtx, remote.Request{Method: http.MethodGet, Path: "/v1/brief?" + params.Encode()}, &raw)
	if err != nil {
		if api.Retryable(err) {
			if text, at, ok := state.LoadBriefCache(projectID); ok {
				fmt.Fprintf(r.deps.Stderr, "Can't reach %s. This is the copy from %s.\n", r.apiHostLabel(), shortTime(at))
				fmt.Fprintln(out, text)
				return nil
			}
		}
		return r.networkError(err)
	}
	if parsed.bools["json"] {
		_, err := fmt.Fprintln(out, string(raw))
		return err
	}
	var response struct {
		Text string `json:"text"`
	}
	decodeInto(raw, &response)
	if !parsed.bools["all"] {
		_ = state.SaveBriefCache(projectID, response.Text)
	}
	_, err = fmt.Fprintln(out, response.Text)
	return err
}
