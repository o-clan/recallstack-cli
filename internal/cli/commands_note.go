package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/secrets"
	"github.com/o-clan/recallstack-cli/internal/state"
)

var noteKinds = map[string]bool{"goal": true, "focus": true, "decision": true, "question": true, "taste": true}

func (r *runtimeServices) runNote(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"why", "quote", "replaces", "answers", "where"}, []string{"json"}).withLists("instead-of"))
	if err != nil {
		return err
	}
	if len(parsed.positionals) < 2 {
		return fmt.Errorf("usage: %s", usage["note"])
	}
	kind := strings.ToLower(parsed.positionals[0])
	if !noteKinds[kind] {
		return fmt.Errorf("the kind is one of goal, focus, decision, question or taste")
	}
	what := strings.TrimSpace(strings.Join(parsed.positionals[1:], " "))
	if what == "" {
		return fmt.Errorf("say what to note")
	}
	where, err := parseWhere(parsed.values["where"])
	if err != nil {
		return err
	}
	if kind == "focus" && where != nil && where["scope"] == "everywhere" {
		return &ExitError{Code: 1, Message: "Focus belongs to a project, so it can't apply everywhere."}
	}
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	if !tc.known && where != nil && where["scope"] == "path" {
		return &ExitError{Code: 1, Message: "This folder isn't a named project yet, so a path can't be scoped. Name it with  recallstack project use \"<name>\""}
	}
	strip := secrets.StripString
	what = strip(what)
	quote := strip(strings.TrimSpace(parsed.values["quote"]))
	why := strip(strings.TrimSpace(parsed.values["why"]))
	insteadOf := []string{}
	for _, option := range parsed.lists["instead-of"] {
		if option = strings.TrimSpace(strip(option)); option != "" {
			insteadOf = append(insteadOf, option)
		}
	}
	if len([]rune(what)) > 300 || len([]rune(why)) > 300 || len(insteadOf) > 5 {
		return &ExitError{Code: 1, Message: "Too long: what and why are at most 300 characters each, with at most 5 --instead-of options."}
	}
	if runes := []rune(quote); len(runes) > 500 {
		quote = string(runes[:499]) + "…"
	}
	changeID := uuid7()
	body := map[string]any{
		"change_id": changeID, "project_id": tc.projectID(), "session_id": tc.sessionID(), "agent": tc.agent(),
		"kind": kind, "what": what, "why": nullable(why), "next": nil, "quote": nullable(quote),
		"instead_of": insteadOf, "replaces": nullable(parsed.values["replaces"]), "answers": nullable(parsed.values["answers"]),
		"where": where, "origin_key": tc.folder.Key,
	}
	var response struct {
		ChangeID    string        `json:"change_id"`
		Record      recordSummary `json:"record"`
		DuplicateOf *string       `json:"duplicate_of"`
	}
	sessionKey := fmt.Sprint(tc.sessionID())
	if tc.sessionID() == nil {
		sessionKey = "hand"
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	queued, err := r.svc.Send(requestCtx, remote.Write{
		Path: "/v1/records", Body: body, Label: "note", ChangeID: changeID,
		IdempotencyKey: "note-" + sha1Text(sessionKey, kind, what, quote), Origin: "note",
	}, &response)
	if err != nil {
		return r.thoughtError(err, tc, parsed.values["replaces"]+parsed.values["answers"])
	}
	change := state.Change{ChangeID: changeID, Kind: "note", What: what}
	if queued {
		if record, ok := r.svc.Queue().FindByChangeID(changeID); ok {
			change.PendingID = record.ID
		}
		_ = state.AddChange(tc.changesKey(), change)
		if parsed.bools["json"] {
			return writeJSON(r.deps.Stdout, map[string]any{"queued": true, "change_id": changeID, "kind": kind, "what": what})
		}
		fmt.Fprintf(r.deps.Stdout, "%sNoted %s (will sync) · %s\n", r.dot(kind, true), kind, what)
		return nil
	}
	if response.ChangeID != "" {
		change.ChangeID = response.ChangeID
	}
	change.Ref = response.Record.Ref
	_ = state.AddChange(tc.changesKey(), change)
	if parsed.bools["json"] {
		return writeJSON(r.deps.Stdout, response)
	}
	shown := response.Record.What
	if shown == "" {
		shown = what
	}
	fmt.Fprintf(r.deps.Stdout, "%sNoted %s %s · %s\n", r.dot(kind, true), kind, response.Record.Ref, shown)
	return nil
}

// thoughtError maps contract errors to the CLI's lines.
func (r *runtimeServices) thoughtError(err error, tc thoughtContext, ref string) error {
	details := api.Details(err)
	if value, ok := details["ref"].(string); ok && value != "" {
		ref = value
	}
	switch api.Code(err) {
	case "REF_NOT_FOUND":
		return &ExitError{Code: 1, Message: fmt.Sprintf("No %s in %s or your taste. Find it by meaning:  recallstack why \"<words>\"", ref, tc.projectName())}
	case "NOT_IN_FORCE":
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s isn't in force anymore. See what happened:  recallstack why %s", ref, ref)}
	case "WAITING_ON_PERSON":
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s is waiting on you. Settle it with recallstack resolve %s <choice>.", ref, ref)}
	case "NO_PROJECT":
		return &ExitError{Code: 1, Message: "This folder isn't a named project yet. Name it with  recallstack project use \"<name>\", or use --where everywhere."}
	case "QUOTE_REQUIRED":
		return &ExitError{Code: 1, Message: "Pass the person's words with --quote \"<their words>\"."}
	case "INVALID_SCOPE":
		return &ExitError{Code: 1, Message: "That --where doesn't fit this thought. Focus can't apply everywhere, and a path needs a project."}
	case "INVALID_REF":
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s isn't a ref. Refs look like D6, T2 or recallstack:D6.", ref)}
	}
	return r.networkError(err)
}

func (r *runtimeServices) runHold(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"plan"}, []string{"json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 || strings.TrimSpace(parsed.values["plan"]) == "" {
		return fmt.Errorf("usage: %s", usage["hold"])
	}
	against := strings.TrimSpace(parsed.positionals[0])
	if !isRef(against) {
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s isn't a ref. Hold on a decision (D1) or a conflict (C1).", against)}
	}
	plan := secrets.StripString(strings.TrimSpace(parsed.values["plan"]))
	if len([]rune(plan)) > 200 {
		plan = string([]rune(plan)[:200])
	}
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	if tc.id.ByHand() {
		return &ExitError{Code: 1, Message: "Recallstack can't tell which agent session this is, so it can't pause edits. Run hold from inside the agent."}
	}
	person := r.person()
	projectID := ""
	if id, ok := tc.projectID().(string); ok {
		projectID = id
	}
	hold := state.Hold{
		ClientRef: "hl_" + randomHex(3), AgainstRef: against, Plan: plan, Person: person,
		SessionID: tc.id.SessionID, Agent: tc.id.Agent, ProjectID: projectID, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if existing, ok := state.LoadHold(tc.id.SessionID); ok && existing.AgainstRef == against {
		hold.ClientRef, hold.Ref = existing.ClientRef, existing.Ref
	}
	// The hold file first, so edits pause even when the server can't be reached.
	if err := state.SaveHold(hold); err != nil {
		return err
	}
	var response struct {
		Hold struct {
			Ref       string `json:"ref"`
			ClientRef string `json:"client_ref"`
		} `json:"hold"`
		Against struct {
			Type     string          `json:"type"`
			Record   json.RawMessage `json:"record"`
			Conflict json.RawMessage `json:"conflict"`
		} `json:"against"`
		Person string `json:"person"`
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	queued, err := r.svc.Send(requestCtx, remote.Write{
		Path: "/v1/holds", Label: "hold", IdempotencyKey: "hold-" + hold.ClientRef, Origin: "hold",
		Body: map[string]any{"client_ref": hold.ClientRef, "project_id": tc.projectID(), "session_id": tc.id.SessionID, "agent": tc.id.Agent, "against_ref": against, "plan": plan},
	}, &response)
	if err != nil {
		state.RemoveHold(tc.id.SessionID)
		if api.Code(err) == "NOT_IN_FORCE" {
			return &ExitError{Code: 1, Message: fmt.Sprintf("%s isn't in force anymore, so there's nothing to hold on. Check what holds now:  recallstack why %s", against, against)}
		}
		return r.thoughtError(err, tc, against)
	}
	out := r.deps.Stdout
	if queued {
		fmt.Fprintf(out, "Holding on %s (will sync). Edits are paused until %s answers.\n", against, person)
		if what, why := briefLines(projectID, against); what != "" {
			fmt.Fprintf(out, "\n  ● %s\n", what)
			if why != "" {
				fmt.Fprintf(out, "    why: %s\n", why)
			}
		}
		return nil
	}
	hold.Ref = response.Hold.Ref
	hold.AgainstType = response.Against.Type
	if response.Person != "" {
		hold.Person = response.Person
		person = response.Person
	}
	hold.Synced = true
	_ = state.SaveHold(hold)
	if parsed.bools["json"] {
		return writeJSON(out, response)
	}
	fmt.Fprintf(out, "Holding on %s. Edits are paused until %s answers.\n", against, person)
	if response.Against.Type == "conflict" {
		var conflict conflictBody
		decodeInto(response.Against.Conflict, &conflict)
		fmt.Fprintln(out)
		width := len([]rune(conflict.A.Show))
		if other := len([]rune(conflict.B.Show)); other > width {
			width = other
		}
		for _, side := range []struct {
			label string
			value conflictSide
		}{{"a", conflict.A}, {"b", conflict.B}} {
			gap := strings.Repeat(" ", width-len([]rune(side.value.Show))+4)
			fmt.Fprintf(out, "  %s  %s%s%s · %s\n", side.label, side.value.Show, gap, agentDisplay(side.value.Agent), dateLabel(side.value.Date, false))
		}
		return nil
	}
	var record recordSummary
	decodeInto(response.Against.Record, &record)
	fmt.Fprintf(out, "\n  ● %s\n", record.What)
	if record.Why != nil && *record.Why != "" {
		if record.WhyIsQuote {
			fmt.Fprintf(out, "    why: \"%s\"\n", *record.Why)
		} else {
			fmt.Fprintf(out, "    why: %s\n", *record.Why)
		}
	}
	if record.Source != nil {
		line := []string{}
		if record.Source.AgentName != "" {
			line = append(line, record.Source.AgentName)
		} else if record.Source.ImportedFrom != nil && record.Source.ImportedFrom.Path != "" {
			// A repo's shared rule file belongs to no one agent; show the file.
			line = append(line, path.Base(record.Source.ImportedFrom.Path))
		}
		if record.Source.SetAt != "" {
			line = append(line, dateLabel(record.Source.SetAt, false))
		}
		if record.Source.Quote != nil && *record.Source.Quote != "" {
			line = append(line, "\""+cutQuote(*record.Source.Quote)+"\"")
		}
		if len(line) > 0 {
			fmt.Fprintf(out, "    %s\n", strings.Join(line, " · "))
		}
	}
	return nil
}

type conflictSide struct {
	Show      string `json:"show"`
	Agent     string `json:"agent"`
	AgentName string `json:"agent_name"`
	Date      string `json:"date"`
	Short     string `json:"short"`
}

type conflictBody struct {
	Ref   string       `json:"ref"`
	Topic string       `json:"topic"`
	State string       `json:"state"`
	A     conflictSide `json:"a"`
	B     conflictSide `json:"b"`
}

func agentDisplay(agent string) string {
	switch agent {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "cursor":
		return "Cursor"
	case "copilot":
		return "Copilot"
	case "antigravity":
		return "Antigravity"
	case "app":
		return "Recallstack app"
	case "hand":
		return "you, in the terminal"
	}
	return agent
}

var holdChoices = map[string]bool{"change-plan": true, "exception": true, "replace": true, "cancel": true}
var conflictChoices = map[string]bool{"a": true, "b": true, "neither": true, "later": true}

func (r *runtimeServices) runResolve(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"quote", "text"}, []string{"json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 2 {
		return fmt.Errorf("usage: %s", usage["resolve"])
	}
	ref, choice := strings.TrimSpace(parsed.positionals[0]), strings.ToLower(strings.TrimSpace(parsed.positionals[1]))
	quote := secrets.StripString(strings.TrimSpace(parsed.values["quote"]))
	text := secrets.StripString(strings.TrimSpace(parsed.values["text"]))
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	byHand := tc.id.ByHand()
	isConflict := strings.HasPrefix(strings.TrimPrefix(ref, refNamespace(ref)), "C")
	sessionHold, holding := state.Hold{}, false
	if !byHand {
		sessionHold, holding = state.LoadHold(tc.id.SessionID)
	}
	if holding && (sessionHold.Ref == ref || sessionHold.ClientRef == ref) && strings.HasPrefix(sessionHold.AgainstRef, "C") {
		// resolve H2 a: a hold on a conflict settles the conflict.
		isConflict = conflictChoices[choice]
		if isConflict {
			ref = sessionHold.AgainstRef
		}
	}
	if isConflict {
		if !conflictChoices[choice] {
			return fmt.Errorf("a conflict is settled with a, b, neither or later")
		}
		if (choice == "a" || choice == "b") && quote == "" && !byHand {
			return &ExitError{Code: 1, Message: "Pass the person's words with --quote \"<their words>\"."}
		}
		if choice == "neither" && (text == "" || (quote == "" && !byHand)) {
			return &ExitError{Code: 1, Message: "neither needs --text \"<what holds instead>\" and --quote \"<their words>\"."}
		}
		return r.resolveConflict(ctx, tc, ref, choice, quote, text, parsed.bools["json"])
	}
	if !holdChoices[choice] {
		return fmt.Errorf("a hold is resolved with change-plan, exception, replace or cancel")
	}
	if (choice == "exception" || choice == "replace") && (text == "" || (quote == "" && !byHand)) {
		return &ExitError{Code: 1, Message: fmt.Sprintf("%s needs --text \"<one line>\" and --quote \"<the exact words>\".", choice)}
	}
	against := ""
	if holding && (sessionHold.Ref == ref || sessionHold.ClientRef == ref) {
		against = sessionHold.AgainstRef
	}
	body := map[string]any{"project_id": tc.projectID(), "session_id": tc.sessionID(), "agent": tc.agent(), "choice": choice, "text": nullable(text), "quote": nullable(quote)}
	changeID := ""
	if choice == "exception" || choice == "replace" {
		changeID = uuid7()
		body["change_id"] = changeID
	}
	var response struct {
		Hold struct {
			Ref   string `json:"ref"`
			State string `json:"state"`
		} `json:"hold"`
		ChangeID  string `json:"change_id"`
		Exception struct {
			Text string `json:"text"`
		} `json:"exception"`
		Decision recordSummary `json:"decision"`
		Upheld   recordSummary `json:"upheld"`
		New      recordSummary `json:"new"`
		Replaced recordSummary `json:"replaced"`
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	queued, err := r.svc.Send(requestCtx, remote.Write{
		Path: "/v1/holds/" + url.PathEscape(ref) + "/resolve", Body: body, Label: "resolve", ChangeID: changeID,
		IdempotencyKey: "resolve-" + ref + "-" + sha1Text(choice, quote, text), Origin: "resolve",
	}, &response)
	if err != nil && api.Code(err) == "HOLD_NOT_OPEN" {
		r.clearHold(tc, ref)
		if still, ok := state.LoadHold(tc.id.SessionID); ok && !byHand {
			return &ExitError{Code: 1, Message: fmt.Sprintf("%s was already settled. This session is still holding on %s:  recallstack resolve %s <choice>", ref, still.AgainstRef, still.DisplayRef())}
		}
		fmt.Fprintf(r.deps.Stdout, "%s was already settled. Edits resume.\n", ref)
		return nil
	}
	if err != nil {
		return r.thoughtError(err, tc, ref)
	}
	r.clearHold(tc, ref)
	out := r.deps.Stdout
	willSync := ""
	if queued {
		willSync = " (will sync)"
	}
	if against == "" {
		against = firstNonEmptyString(response.Decision.Ref, response.Upheld.Ref, response.Replaced.Ref, "the decision")
	}
	if changeID != "" {
		change := state.Change{ChangeID: firstNonEmptyString(response.ChangeID, changeID), Kind: choice, Ref: against}
		if queued {
			if record, ok := r.svc.Queue().FindByChangeID(changeID); ok {
				change.PendingID = record.ID
			}
		}
		if choice == "replace" && response.New.Ref != "" {
			change.Ref, change.What = response.New.Ref, response.New.What
		}
		_ = state.AddChange(tc.changesKey(), change)
	}
	if parsed.bools["json"] && !queued {
		return writeJSON(out, response)
	}
	switch choice {
	case "change-plan":
		fmt.Fprintf(out, "%s kept%s. Edits resume.\n", against, willSync)
	case "exception":
		fmt.Fprintf(out, "Exception to %s noted%s: %s. %s stays in force. Edits resume.\n", against, willSync, strings.TrimSuffix(text, "."), against)
	case "replace":
		if queued || response.New.Ref == "" {
			fmt.Fprintf(out, "%s replaced%s: %s. Edits resume.\n", against, willSync, strings.TrimSuffix(text, "."))
		} else {
			fmt.Fprintf(out, "%s replaced by %s: %s. Edits resume.\n", against, response.New.Ref, strings.TrimSuffix(text, "."))
		}
	case "cancel":
		fmt.Fprintf(out, "Hold cancelled%s. Edits resume.\n", willSync)
	}
	return nil
}

func refNamespace(ref string) string {
	if index := strings.Index(ref, ":"); index >= 0 {
		return ref[:index+1]
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// clearHold removes this session's hold file when it matches ref (or the
// conflict ref it holds on).
func (r *runtimeServices) clearHold(tc thoughtContext, ref string) {
	if tc.id.ByHand() {
		return
	}
	if hold, ok := state.LoadHold(tc.id.SessionID); ok && (hold.Ref == ref || hold.ClientRef == ref || hold.AgainstRef == ref) {
		state.RemoveHold(tc.id.SessionID)
	}
}

func (r *runtimeServices) resolveConflict(ctx context.Context, tc thoughtContext, ref, choice, quote, text string, asJSON bool) error {
	changeID := ""
	body := map[string]any{"project_id": tc.projectID(), "session_id": tc.sessionID(), "agent": tc.agent(), "choice": choice, "quote": nullable(quote), "text": nullable(text)}
	if choice != "later" {
		changeID = uuid7()
		body["change_id"] = changeID
	}
	var response struct {
		Conflict    conflictBody `json:"conflict"`
		ChangeID    string       `json:"change_id"`
		ClosedHolds []string     `json:"closed_holds"`
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	queued, err := r.svc.Send(requestCtx, remote.Write{
		Path: "/v1/conflicts/" + url.PathEscape(ref) + "/resolve", Body: body, Label: "conflict", ChangeID: changeID,
		IdempotencyKey: "resolve-" + ref + "-" + sha1Text(choice, quote, text), Origin: "resolve",
	}, &response)
	if err != nil {
		if api.Code(err) == "ALREADY_RESOLVED" {
			r.clearHold(tc, ref)
			fmt.Fprintf(r.deps.Stdout, "%s was already settled. Edits resume.\n", ref)
			return nil
		}
		return r.thoughtError(err, tc, ref)
	}
	r.clearHold(tc, ref)
	if changeID != "" {
		_ = state.AddChange(tc.changesKey(), state.Change{ChangeID: firstNonEmptyString(response.ChangeID, changeID), Kind: "conflict_settle", Ref: ref})
	}
	out := r.deps.Stdout
	if asJSON && !queued {
		return writeJSON(out, response)
	}
	switch {
	case choice == "later":
		fmt.Fprintln(out, "Left for later. Edits resume.")
	case queued:
		fmt.Fprintf(out, "Settled %s (will sync). Edits resume.\n", ref)
	case choice == "neither":
		fmt.Fprintf(out, "Settled: %s. Both earlier rules replaced.\n", strings.TrimSuffix(text, "."))
	default:
		kept, aside := response.Conflict.A, response.Conflict.B
		if choice == "b" {
			kept, aside = aside, kept
		}
		fmt.Fprintf(out, "Settled: %s. %s %s set aside: \"%s\"\n", kept.Short, dateLabel(aside.Date, false), aside.Short, cutQuote(quote))
	}
	return nil
}
