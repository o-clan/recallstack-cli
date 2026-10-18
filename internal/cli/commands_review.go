package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/state"
)

// recallstack review: what needs the person, settled one key at a time.
// Until the Thoughts app ships, this is where imports kept for review,
// agent-proposed and inferred thoughts, "still right?" questions, holds left
// unanswered and open conflicts get settled.

type reviewRecord struct {
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	What      string `json:"what"`
	State     string `json:"state"`
	Certainty string `json:"certainty"`
	Imported  bool   `json:"imported"`
	Scope     struct {
		Display string `json:"display"`
	} `json:"scope"`
	Why          *string `json:"why"`
	Quote        *string `json:"quote"`
	AgentName    string  `json:"agent_name"`
	Date         string  `json:"date"`
	SeenCount    int     `json:"seen_count"`
	ImportedFrom *struct {
		Path      string `json:"path"`
		LineStart int    `json:"line_start"`
		ImportID  string `json:"import_id"`
	} `json:"imported_from"`
	Exceptions []struct {
		Text  string `json:"text"`
		SetAt string `json:"set_at"`
	} `json:"exceptions"`
}

type reviewItem struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Group    string        `json:"group"`
	Text     string        `json:"text"`
	Choices  []string      `json:"choices"`
	Record   *reviewRecord `json:"record"`
	Conflict *conflictBody `json:"conflict"`
	CloseTo  *struct {
		Ref      string `json:"ref"`
		What     string `json:"what"`
		Relation string `json:"relation"`
	} `json:"close_to"`
	Plan         *string `json:"plan"`
	WhatHappened *string `json:"what_happened"`
}

type reviewList struct {
	Count int          `json:"count"`
	Items []reviewItem `json:"items"`
}

type settleResponse struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Result   string `json:"result"`
	ChangeID string `json:"change_id"`
	Count    int    `json:"count"`
	Record   *struct {
		Ref  string `json:"ref"`
		What string `json:"what"`
	} `json:"record"`
	Previous *struct {
		What  string `json:"what"`
		Scope struct {
			Display string `json:"display"`
		} `json:"scope"`
	} `json:"previous"`
	Detail   string        `json:"detail"`
	Conflict *conflictBody `json:"conflict"`
	New      *struct {
		Ref  string `json:"ref"`
		What string `json:"what"`
	} `json:"new"`
}

func (r *runtimeServices) runReview(args []string) error {
	if len(args) > 0 && args[0] == "settle" {
		return r.runReviewSettle(args[1:])
	}
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"all", "json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) > 0 {
		return fmt.Errorf("usage: %s", usage["review"])
	}
	if !r.svc.SignedIn() {
		return &ExitError{Code: 1, Message: "Sign in first, so Recallstack can show what needs you:  recallstack login"}
	}
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	all := parsed.bools["all"]
	list, raw, err := r.fetchReview(ctx, tc, all)
	if err != nil {
		return r.networkError(err)
	}
	out := r.deps.Stdout
	if parsed.bools["json"] {
		_, err := fmt.Fprintln(out, string(raw))
		return err
	}
	where := ""
	if tc.projectID() != nil && !all {
		where = " in " + tc.projectName()
	}
	if len(list.Items) == 0 {
		fmt.Fprintf(out, "Nothing needs you%s.\n", where)
		return nil
	}
	if !r.interactive() {
		fmt.Fprintf(out, "%s%s\n", needsYouCount(list.Count), where)
		shownGroups := map[string]bool{}
		for _, item := range list.Items {
			fmt.Fprintln(out)
			r.printReviewItem(out, item, list, shownGroups)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Settle them one key at a time in a terminal:  recallstack review")
		return nil
	}
	return r.reviewLoop(ctx, tc, all, list, where)
}

func needsYouCount(n int) string {
	if n == 1 {
		return "1 thing needs you"
	}
	return fmt.Sprintf("%d things need you", n)
}

func (r *runtimeServices) fetchReview(ctx context.Context, tc thoughtContext, all bool) (reviewList, json.RawMessage, error) {
	params := url.Values{}
	if id, ok := tc.projectID().(string); ok && id != "" {
		params.Set("project_id", id)
	}
	if all {
		params.Set("all", "1")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var raw json.RawMessage
	err := r.svc.Do(requestCtx, remote.Request{Method: http.MethodGet, Path: "/v1/review?" + params.Encode()}, &raw)
	var list reviewList
	decodeInto(raw, &list)
	return list, raw, err
}

// importCount is how many of an import's thoughts are waiting.
func importCount(list reviewList, group string) int {
	n := 0
	for _, item := range list.Items {
		if item.Group == group {
			n++
		}
	}
	return n
}

func (r *runtimeServices) printReviewItem(out io.Writer, item reviewItem, list reviewList, shownGroups map[string]bool) {
	rec := item.Record
	switch item.Type {
	case "conflict":
		c := item.Conflict
		if c == nil {
			return
		}
		fmt.Fprintf(out, "%s %s rules disagree:\n", c.Ref, c.Topic)
		fmt.Fprintf(out, "   a  %s  (%s, %s)\n", c.A.Show, firstNonEmptyString(c.A.AgentName, agentDisplay(c.A.Agent)), dateLabel(c.A.Date, false))
		fmt.Fprintf(out, "   b  %s  (%s, %s)\n", c.B.Show, firstNonEmptyString(c.B.AgentName, agentDisplay(c.B.Agent)), dateLabel(c.B.Date, false))
		fmt.Fprintln(out, "   Agents ask you before work that touches it, until you settle it.")
	case "import":
		if rec == nil {
			return
		}
		if !shownGroups[item.Group] {
			shownGroups[item.Group] = true
			from := "a file"
			if rec.ImportedFrom != nil {
				from = rec.ImportedFrom.Path
			}
			fmt.Fprintf(out, "Imported from %s, kept for review (%d)\n", from, importCount(list, item.Group))
		}
		fmt.Fprintf(out, "%s%s %s\n", r.dot(rec.Kind, true), rec.Ref, rec.What)
		meta := []string{rec.Kind}
		if rec.ImportedFrom != nil && rec.ImportedFrom.LineStart > 0 {
			meta = append(meta, fmt.Sprintf("line %d", rec.ImportedFrom.LineStart))
		}
		meta = append(meta, rec.Scope.Display)
		fmt.Fprintf(out, "   %s\n", strings.Join(meta, " · "))
	case "proposed", "inferred":
		if rec == nil {
			return
		}
		fmt.Fprintf(out, "%s%s %s\n", r.dot(rec.Kind, true), rec.Ref, rec.What)
		if rec.Why != nil && *rec.Why != "" {
			fmt.Fprintf(out, "   why: %s\n", *rec.Why)
		}
		how := firstNonEmptyString(rec.AgentName, "An agent") + " proposed · " + dateLabel(rec.Date, false)
		if rec.AgentName == "you, in the terminal" {
			how = "noted in the terminal without your words · " + dateLabel(rec.Date, false)
		}
		if item.Type == "inferred" {
			how = "inferred"
			if rec.SeenCount > 1 {
				how = fmt.Sprintf("inferred, seen %d times", rec.SeenCount)
			}
		}
		fmt.Fprintf(out, "   %s · %s · not confirmed\n", how, rec.Scope.Display)
		if item.CloseTo != nil {
			label := map[string]string{"same": "says the same as", "replaces": "would replace", "contradicts": "disagrees with"}[item.CloseTo.Relation]
			if label == "" {
				label = "is close to"
			}
			fmt.Fprintf(out, "   %s %s %s\n", label, item.CloseTo.Ref, item.CloseTo.What)
		}
	default: // exceptions, acted_against, hold_unanswered
		if rec == nil {
			return
		}
		fmt.Fprintln(out, item.Text)
		fmt.Fprintf(out, "   %s%s %s\n", r.dot(rec.Kind, true), rec.Ref, rec.What)
		if rec.Why != nil && *rec.Why != "" {
			fmt.Fprintf(out, "      why: %s\n", *rec.Why)
		}
		if item.Type == "exceptions" {
			for _, exception := range rec.Exceptions {
				fmt.Fprintf(out, "      exception: %s (%s)\n", exception.Text, dateLabel(exception.SetAt, false))
			}
		}
		if item.Plan != nil && *item.Plan != "" {
			fmt.Fprintf(out, "      plan it held: %s\n", *item.Plan)
		}
		if item.WhatHappened != nil && *item.WhatHappened != "" {
			fmt.Fprintf(out, "      what happened: %s\n", *item.WhatHappened)
		}
	}
}

// keysFor is the one-key menu for an item, matching the Thoughts designs' words.
func keysFor(item reviewItem, list reviewList) []reviewKey {
	switch item.Type {
	case "conflict":
		return []reviewKey{{'a', "a", "this one holds"}, {'b', "b", "that one holds"}, {'n', "neither", "neither: write what's true"}}
	case "proposed":
		return []reviewKey{{'c', "confirm", "keep"}, {'n', "adjust", "not quite"}, {'d', "drop", "drop"}}
	case "inferred":
		return []reviewKey{{'c', "confirm", "that's me"}, {'n', "adjust", "not quite"}, {'d', "drop", "not me"}}
	case "import":
		keys := []reviewKey{{'c', "confirm", "confirm"}}
		if n := importCount(list, item.Group); n > 1 {
			keys = append(keys, reviewKey{'a', "confirm_all", fmt.Sprintf("confirm all %d", n)})
		}
		return append(keys, reviewKey{'n', "adjust", "not quite"}, reviewKey{'d', "drop", "drop"})
	}
	return []reviewKey{{'c', "confirm", "still right"}, {'n', "adjust", "not quite"}, {'d', "drop", "drop it"}}
}

type reviewKey struct {
	key    byte
	choice string
	label  string
}

type settled struct {
	item     reviewItem
	changeID string
	ref      string
	what     string
}

func (r *runtimeServices) reviewLoop(ctx context.Context, tc thoughtContext, all bool, list reviewList, where string) error {
	out := r.deps.Stdout
	in, _ := r.deps.Stdin.(*os.File)
	fmt.Fprintf(out, "%s%s\n", needsYouCount(list.Count), where)
	var last *settled
	skipped := map[string]bool{}
	shownGroups := map[string]bool{}
	for {
		var item *reviewItem
		for index := range list.Items {
			if !skipped[list.Items[index].ID] {
				item = &list.Items[index]
				break
			}
		}
		if item == nil {
			fmt.Fprintln(out)
			if len(skipped) > 0 {
				fmt.Fprintf(out, "%d left for later. Run recallstack review again any time.\n", len(skipped))
			} else {
				fmt.Fprintln(out, "That's everything. Nothing else needs you.")
			}
			return nil
		}
		fmt.Fprintln(out)
		r.printReviewItem(out, *item, list, shownGroups)
		keys := keysFor(*item, list)
		menu := []string{}
		for _, key := range keys {
			menu = append(menu, string(key.key)+" "+key.label)
		}
		menu = append(menu, "s skip")
		if last != nil {
			menu = append(menu, "u undo")
		}
		menu = append(menu, "q quit")
		fmt.Fprintf(out, "   %s  ", strings.Join(menu, "   "))
		key, err := readKey(in)
		if err != nil || key == 3 || key == 4 || key == 'q' {
			fmt.Fprintln(out)
			return nil
		}
		if key >= 'A' && key <= 'Z' {
			key += 'a' - 'A'
		}
		fmt.Fprintf(out, "%c\n", key)
		switch key {
		case 's':
			skipped[item.ID] = true
			continue
		case 'u':
			if last == nil {
				continue
			}
			if err := r.undoSettle(ctx, tc, *last); err != nil {
				fmt.Fprintf(out, "   %s\n", errorText(err))
				continue
			}
			last = nil
			fresh, _, err := r.fetchReview(ctx, tc, all)
			if err != nil {
				return r.networkError(err)
			}
			list = fresh
			shownGroups = map[string]bool{}
			continue
		}
		var chosen *reviewKey
		for index := range keys {
			if keys[index].key == key {
				chosen = &keys[index]
			}
		}
		if chosen == nil {
			fmt.Fprintln(out, "   Press one of the keys shown.")
			continue
		}
		body := map[string]any{"choice": chosen.choice}
		switch {
		case chosen.choice == "adjust":
			text, _ := promptLine(r.deps.Stdin, out, "   What should it say? Enter keeps the words.  > ")
			whereText, _ := promptLine(r.deps.Stdin, out, "   Where does it apply? everywhere, project, project:<name> or path:<path>. Enter keeps it.  > ")
			if text == "" && whereText == "" {
				fmt.Fprintln(out, "   Nothing changed.")
				continue
			}
			if text != "" {
				body["text"] = text
			}
			if whereText != "" {
				scope, err := parseWhere(whereText)
				if err != nil {
					fmt.Fprintf(out, "   %s\n", err.Error())
					continue
				}
				body["where"] = scope
			}
		case chosen.choice == "neither":
			text, _ := promptLine(r.deps.Stdin, out, "   What holds instead?  > ")
			if text == "" {
				fmt.Fprintln(out, "   Nothing changed.")
				continue
			}
			body["text"] = text
		}
		response, err := r.settleReviewItem(ctx, tc, item.ID, body)
		if err != nil {
			fmt.Fprintf(out, "   %s\n", errorText(err))
			if api.Code(err) == "ALREADY_RESOLVED" || api.Code(err) == "NOT_IN_FORCE" || api.Code(err) == "NOT_FOUND" {
				skipped[item.ID] = true
			}
			continue
		}
		fmt.Fprintf(out, "   %s\n", settledLine(*item, chosen.choice, response))
		last = &settled{item: *item, changeID: response.ChangeID}
		if response.Record != nil {
			last.ref, last.what = response.Record.Ref, response.Record.What
		}
		fresh, _, err := r.fetchReview(ctx, tc, all)
		if err != nil {
			return r.networkError(err)
		}
		list = fresh
	}
}

func errorText(err error) string {
	if message := api.Message(err); message != "" {
		return message
	}
	return err.Error()
}

func (r *runtimeServices) settleReviewItem(ctx context.Context, tc thoughtContext, id string, body map[string]any) (settleResponse, error) {
	changeID := uuid7()
	body["change_id"] = changeID
	body["project_id"] = tc.projectID()
	body["agent"] = "hand"
	var response settleResponse
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := r.svc.Do(requestCtx, remote.Request{Method: http.MethodPost, Path: "/v1/review/" + url.PathEscape(id) + "/settle", Body: body, IdempotencyKey: changeID}, &response)
	if err != nil {
		return response, err
	}
	if response.ChangeID == "" {
		response.ChangeID = changeID
	}
	kind := map[string]string{"confirmed": "confirm", "kept": "confirm", "adjusted": "adjust", "dropped": "drop", "settled": "conflict_settle"}[response.Result]
	ref, what := "", ""
	if response.Record != nil {
		ref, what = response.Record.Ref, response.Record.What
	}
	// By hand, so recallstack undo in a terminal reaches it too.
	_ = state.AddChange("", state.Change{ChangeID: response.ChangeID, Kind: kind, Ref: ref, What: what})
	return response, nil
}

func (r *runtimeServices) undoSettle(ctx context.Context, tc thoughtContext, last settled) error {
	undoID := uuid7()
	body := map[string]any{"change_id": undoID, "project_id": tc.projectID(), "session_id": nil, "agent": "hand"}
	var response struct {
		Undone struct {
			Kind  string `json:"kind"`
			Ref   string `json:"ref"`
			What  string `json:"what"`
			Count *int   `json:"count"`
		} `json:"undone"`
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path := "/v1/changes/" + url.PathEscape(last.changeID) + "/undo"
	if err := r.svc.Do(requestCtx, remote.Request{Method: http.MethodPost, Path: path, Body: body, IdempotencyKey: "undo-" + last.changeID + "-" + randomHex(4)}, &response); err != nil {
		return err
	}
	_ = state.UpdateChange("", last.changeID, func(c *state.Change) { c.Undone = true })
	out := r.deps.Stdout
	switch {
	case last.item.Type == "conflict" && last.item.Conflict != nil:
		fmt.Fprintf(out, "   Undid settling %s. Both sides are back, and it's open again.\n", last.item.Conflict.Ref)
		return nil
	case response.Undone.Count != nil && *response.Undone.Count > 1:
		fmt.Fprintf(out, "   Undid confirming %d thoughts. Back in review.\n", *response.Undone.Count)
		return nil
	}
	verb := undoVerbs[response.Undone.Kind]
	if verb == "" {
		verb = response.Undone.Kind
	}
	what := firstNonEmptyString(response.Undone.What, last.what)
	ref := firstNonEmptyString(response.Undone.Ref, last.ref)
	fmt.Fprintf(out, "   Undid %s %s · %s Back in review.\n", verb, ref, sentence(firstSentence(what)))
	return nil
}

// settledLine is the one line after a settle.
func settledLine(item reviewItem, choice string, response settleResponse) string {
	ref, what := "", ""
	if response.Record != nil {
		ref, what = response.Record.Ref, response.Record.What
	}
	if ref == "" && item.Record != nil {
		ref, what = item.Record.Ref, item.Record.What
	}
	short := sentence(firstSentence(what))
	switch {
	case item.Type == "conflict" && choice == "neither":
		text := ""
		if response.New != nil {
			text = response.New.What
		}
		return fmt.Sprintf("Settled: %s Both earlier rules replaced.", sentence(text))
	case item.Type == "conflict":
		if response.Conflict == nil {
			return "Settled."
		}
		kept, aside := response.Conflict.A, response.Conflict.B
		if choice == "b" {
			kept, aside = aside, kept
		}
		return fmt.Sprintf("Settled: %s. %s %s set aside.", kept.Short, dateLabel(aside.Date, false), aside.Short)
	case choice == "confirm_all":
		return fmt.Sprintf("Confirmed %d imported thoughts. They're in the next brief.", response.Count)
	case choice == "confirm" && (item.Type == "exceptions" || item.Type == "acted_against" || item.Type == "hold_unanswered"):
		return fmt.Sprintf("Kept %s · %s Still in force.", ref, short)
	case choice == "confirm":
		return fmt.Sprintf("Confirmed %s · %s It's in the next brief.", ref, short)
	case choice == "adjust":
		parts := []string{}
		if response.Previous != nil && response.Previous.What != what {
			parts = append(parts, fmt.Sprintf("now \"%s\", was \"%s\"", what, response.Previous.What))
		}
		if response.Previous != nil && strings.Contains(response.Detail, ", was ") {
			parts = append(parts, strings.SplitN(response.Detail, ";", 2)[0])
		}
		line := "Adjusted " + ref
		if len(parts) > 0 {
			line += " · " + strings.Join(parts, "; ")
		}
		if item.Type == "proposed" || item.Type == "inferred" || item.Type == "import" {
			return line + ". Confirmed, so it's in the next brief."
		}
		return line + "."
	case choice == "drop":
		return fmt.Sprintf("Dropped %s · %s Kept in history, left out of briefs.", ref, short)
	}
	return "Done."
}

// recallstack review settle <id> <choice> [--text] [--where] [--why]: the same
// settles, for scripts.
func (r *runtimeServices) runReviewSettle(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"text", "where", "why"}, []string{"json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 2 {
		return fmt.Errorf("usage: %s", usage["review settle"])
	}
	id, choice := parsed.positionals[0], parsed.positionals[1]
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	list, _, err := r.fetchReview(ctx, tc, true)
	if err != nil {
		return r.networkError(err)
	}
	var item *reviewItem
	for index := range list.Items {
		if list.Items[index].ID == id {
			item = &list.Items[index]
		}
	}
	if item == nil {
		return &ExitError{Code: 1, Message: "That isn't waiting in review anymore. See what is:  recallstack review"}
	}
	body := map[string]any{"choice": choice}
	if value := parsed.values["text"]; value != "" {
		body["text"] = value
	}
	if value := parsed.values["why"]; value != "" {
		body["why"] = value
	}
	if value := parsed.values["where"]; value != "" {
		scope, err := parseWhere(value)
		if err != nil {
			return err
		}
		body["where"] = scope
	}
	response, err := r.settleReviewItem(ctx, tc, id, body)
	if err != nil {
		if message := api.Message(err); message != "" {
			return &ExitError{Code: 1, Message: message}
		}
		return r.networkError(err)
	}
	if parsed.bools["json"] {
		return writeJSON(r.deps.Stdout, response)
	}
	fmt.Fprintln(r.deps.Stdout, settledLine(*item, choice, response))
	fmt.Fprintln(r.deps.Stdout, "Take it back:  recallstack undo")
	return nil
}

// readKeyLine is the fallback for readKey: the first character of a line.
func readKeyLine(in *os.File) (byte, error) {
	buffer := make([]byte, 256)
	n, err := in.Read(buffer)
	if err != nil {
		return 0, err
	}
	line := strings.TrimSpace(string(buffer[:n]))
	if line == "" {
		return '\n', nil
	}
	return line[0], nil
}
