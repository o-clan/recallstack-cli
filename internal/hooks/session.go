package hooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/identity"
	"github.com/o-clan/recallstack-cli/internal/integrations"
	"github.com/o-clan/recallstack-cli/internal/pending"
	"github.com/o-clan/recallstack-cli/internal/project"
	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/state"
)

// Budgets from spec section 6.
const (
	startNetworkBudget  = 1350 * time.Millisecond // 1.5 s total, 1.2 s for the request itself
	startRequestBudget  = 1200 * time.Millisecond
	promptNetworkBudget = 750 * time.Millisecond // 0.8 s total
	endNetworkBudget    = 1500 * time.Millisecond
)

type sessionResponse struct {
	Session struct {
		ID             string `json:"id"`
		AgentSessionID string `json:"agent_session_id"`
		ProjectID      string `json:"project_id"`
	} `json:"session"`
	Kind        string  `json:"kind"`
	V           int     `json:"v"`
	Text        *string `json:"text"`
	VisibleLine string  `json:"visible_line"`
	NeedsYou    int     `json:"needs_you"`
	Tokens      int     `json:"tokens"`
}

type startResult struct {
	response  sessionResponse
	entry     project.Entry
	known     bool
	signedOut bool
	offline   bool
	err       error
}

func (h *hookRun) sessionStart(ctx context.Context) string {
	sessionID := h.sessionID()
	if sessionID == "" {
		return "no_session_id"
	}
	source := firstNonEmpty(stringValue(h.payload["source"]), "startup")
	if source == "fork" {
		source = "startup"
	}
	folder := project.Detect(h.cwd)
	entry, known := project.Lookup(folder.Key)

	session, _ := state.LoadSession(h.agent, sessionID)
	session.Agent, session.SessionID = h.agent, sessionID
	session.CWD, session.CWDKey, session.RepoRoot = folder.CWD, folder.Key, folder.RepoRoot
	session.Source = source
	session.TranscriptPath = firstNonEmpty(stringValue(h.payload["transcript_path"]), session.TranscriptPath)
	session.EndedAt = ""
	if session.StartedAt == "" || source == "startup" {
		session.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if known {
		session.ProjectID, session.ProjectName = entry.ProjectID, entry.Name
	}
	if pid, err := identity.WriteLink(h.agent, sessionID, folder.CWD); err == nil && pid > 0 {
		session.AgentPID = pid
	}
	_ = state.SaveSession(session)
	if h.agent == "claude" {
		writeClaudeEnv(sessionID)
	}

	deadline := h.started.Add(startNetworkBudget)
	networkCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	results := make(chan startResult, 1)
	go func() { results <- h.fetchStart(networkCtx, folder, entry, known, session, source) }()
	var result startResult
	select {
	case result = <-results:
	case <-networkCtx.Done():
		result = startResult{offline: true, entry: entry, known: known}
	}
	if result.known && result.entry.ProjectID != "" {
		entry, known = result.entry, true
		session.ProjectID, session.ProjectName = entry.ProjectID, entry.Name
	}

	name := folder.Name
	if known && entry.Name != "" {
		name = entry.Name
	} else if !folder.InRepo {
		name = "not named yet"
	}
	context := ""
	visible := ""
	outcome := "ok"
	switch {
	case result.signedOut:
		visible = fmt.Sprintf("Recallstack · %s · signed out, run recallstack login", name)
		outcome = "signed_out"
	case result.offline || result.err != nil:
		outcome = "offline"
		if result.err != nil {
			outcome = "error: " + truncate(result.err.Error(), 200)
		}
		if cached, at, ok := state.LoadBriefCache(session.ProjectID); ok {
			when := clockLabel(at)
			context = withOfflineNote(cached, when)
			// The prompt hook asks for changes since this copy once the API is back.
			if match := briefVersion.FindStringSubmatch(cached); match != nil && session.V == 0 {
				session.V, _ = strconv.Atoi(match[1])
			}
			visible = fmt.Sprintf("Recallstack · %s · offline copy from %s", name, when)
		} else {
			visible = fmt.Sprintf("Recallstack · %s · offline, no copy of the brief yet", name)
		}
	default:
		response := result.response
		if response.Text != nil {
			context = *response.Text
		}
		if response.Kind != "update" && context != "" {
			_ = state.SaveBriefCache(session.ProjectID, context)
		}
		session.V = response.V
		session.NeedsYou = response.NeedsYou
		session.LastBriefAt = time.Now().UTC().Format(time.RFC3339)
		visible = response.VisibleLine
		if visible == "" {
			visible = "Recallstack · " + name
		}
	}
	if suffix := importHint(folder); suffix != "" {
		visible += suffix
	}
	if h.agent == "codex" {
		visible += codexTrustHint(folder)
	}
	_ = state.SaveSession(session)
	h.contextOutput("SessionStart", context, visible)
	if h.agent == "copilot" && context != "" {
		_ = writeCopilotBrief(context)
	}
	return outcome
}

func (h *hookRun) fetchStart(ctx context.Context, folder project.Folder, entry project.Entry, known bool, session state.Session, source string) startResult {
	svc := remote.New(h.version)
	if !svc.SignedIn() {
		return startResult{signedOut: true, entry: entry, known: known}
	}
	if !known && folder.InRepo {
		resolved, _, err := project.Resolve(ctx, svc, folder)
		if err == nil && resolved.ProjectID != "" {
			entry, known = resolved, true
		} else if err != nil && pending.IsAuthRequired(err) {
			return startResult{signedOut: true, entry: entry, known: known}
		} else if err != nil {
			return startResult{offline: true, entry: entry, known: known}
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, startRequestBudget)
	defer cancel()
	var projectID any
	if known && entry.ProjectID != "" {
		projectID = entry.ProjectID
	}
	var response sessionResponse
	err := svc.Do(requestCtx, remote.Request{
		Method: http.MethodPost, Path: "/v1/sessions",
		IdempotencyKey: fmt.Sprintf("session-%s-%s-%s-%d", h.agent, session.SessionID, source, time.Now().Unix()/60),
		Body: map[string]any{
			"session_id": session.SessionID, "agent": h.agent, "project_id": projectID,
			"source": source, "cwd_key": folder.Key,
		},
	}, &response)
	if err != nil {
		if pending.IsAuthRequired(err) {
			return startResult{signedOut: true, entry: entry, known: known}
		}
		return startResult{offline: true, entry: entry, known: known, err: nil}
	}
	return startResult{response: response, entry: entry, known: known}
}

var briefVersion = regexp.MustCompile(`<recallstack_brief[^>]* v="(\d+)"`)

// withOfflineNote appends " (offline copy from HH:MM)" to the "Brief for…" line.
func withOfflineNote(brief, when string) string {
	lines := strings.Split(brief, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "Brief for ") {
			lines[index] = line + " (offline copy from " + when + ")"
			return strings.Join(lines, "\n")
		}
	}
	return brief
}

func clockLabel(at time.Time) string {
	local := at.Local()
	if local.Format("2006-01-02") == time.Now().Format("2006-01-02") {
		return local.Format("15:04")
	}
	return local.Format("Jan 2 15:04")
}

func writeClaudeEnv(sessionID string) {
	path := strings.TrimSpace(os.Getenv("CLAUDE_ENV_FILE"))
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	quoted := "'" + strings.ReplaceAll(sessionID, "'", `'"'"'`) + "'"
	fmt.Fprintf(file, "export RECALLSTACK_SESSION_ID=%s\nexport RECALLSTACK_AGENT=claude\n", quoted)
}

// codexTrustHint is " · {n} Recallstack hooks wait for you in Codex /hooks"
// when this session started but Codex is still skipping some of Recallstack's
// other hooks (new, changed since they were trusted, or turned off). Codex
// writes that trust state itself; Recallstack only reads it.
func codexTrustHint(folder project.Folder) string {
	root := folder.RepoRoot
	if root == "" {
		root = folder.CWD
	}
	manager, err := integrations.New(root)
	if err != nil {
		return ""
	}
	waiting := 0
	scopes := []integrations.Scope{integrations.ScopeGlobal}
	if folder.RepoRoot != "" {
		scopes = append(scopes, integrations.ScopeLocal)
	}
	for _, scope := range scopes {
		if trust, ok := manager.CodexHooksTrust(scope); ok {
			waiting += trust.Waiting + trust.Disabled
		}
	}
	switch {
	case waiting == 1:
		return " · 1 Recallstack hook waits for you in Codex /hooks"
	case waiting > 1:
		return fmt.Sprintf(" · %d Recallstack hooks wait for you in Codex /hooks", waiting)
	}
	return ""
}

// RuleFiles are the rule files that import reads at a repo root.
var RuleFiles = []string{"AGENTS.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md", ".cursorrules", filepath.Join(".github", "copilot-instructions.md")}

// importHint returns " · {file} here isn't imported, run recallstack import"
// the first time a session starts in a repo whose rule files were never
// imported, and "" every time after.
func importHint(folder project.Folder) string {
	if !folder.InRepo {
		return ""
	}
	var hints map[string]bool
	state.ReadJSON(state.ImportHintsPath(), &hints)
	if hints[folder.RepoRoot] {
		return ""
	}
	var imports struct {
		Files map[string]any `json:"files"`
	}
	state.ReadJSON(state.ImportsPath(), &imports)
	found := ""
	for _, name := range RuleFiles {
		path := filepath.Join(folder.RepoRoot, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			if _, imported := imports.Files[path]; !imported {
				found = name
				break
			}
		}
	}
	if found == "" {
		if matches, _ := filepath.Glob(filepath.Join(folder.RepoRoot, ".cursor", "rules", "*.mdc")); len(matches) > 0 {
			for _, match := range matches {
				if strings.HasPrefix(filepath.Base(match), "recallstack") {
					continue
				}
				if _, imported := imports.Files[match]; !imported {
					found = filepath.Join(".cursor", "rules", filepath.Base(match))
					break
				}
			}
		}
	}
	if found == "" {
		return ""
	}
	if hints == nil {
		hints = map[string]bool{}
	}
	hints[folder.RepoRoot] = true
	_ = state.WriteJSON(state.ImportHintsPath(), hints)
	return " · " + found + " here isn't imported, run recallstack import"
}

type changesResponse struct {
	V             int     `json:"v"`
	Text          *string `json:"text"`
	NeedsYou      *int    `json:"needs_you"`
	ResolvedHolds []struct {
		Ref       string `json:"ref"`
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	} `json:"resolved_holds"`
}

func (h *hookRun) promptSubmit(ctx context.Context) string {
	sessionID := h.sessionID()
	if sessionID == "" {
		return "no_session_id"
	}
	session, known := state.LoadSession(h.agent, sessionID)
	update := ""
	visible := ""
	outcome := "ok"
	// A session that started offline has only its cached copy's version; it
	// still catches up once the API answers again.
	if known && (session.LastBriefAt != "" || session.V > 0) {
		deadline := h.started.Add(promptNetworkBudget)
		networkCtx, cancel := context.WithDeadline(ctx, deadline)
		type changesResult struct {
			status   int
			response changesResponse
			err      error
		}
		results := make(chan changesResult, 1)
		go func() {
			svc := remote.New(h.version)
			if !svc.SignedIn() {
				results <- changesResult{err: errors.New("signed out")}
				return
			}
			query := url.Values{}
			if session.ProjectID != "" {
				query.Set("project_id", session.ProjectID)
			}
			query.Set("since", strconv.Itoa(session.V))
			query.Set("session_id", sessionID)
			query.Set("agent", h.agent)
			var response changesResponse
			err := svc.Do(networkCtx, remote.Request{
				Method: http.MethodGet, Path: "/v1/brief/changes?" + query.Encode(),
				Headers: map[string]string{"If-None-Match": fmt.Sprintf(`"v%d"`, session.V)},
			}, &response)
			if api.Status(err) == http.StatusNotModified {
				results <- changesResult{status: 304}
				return
			}
			results <- changesResult{status: 200, response: response, err: err}
		}()
		var result changesResult
		select {
		case result = <-results:
		case <-networkCtx.Done():
			result = changesResult{err: networkCtx.Err()}
		}
		cancel()
		switch {
		case result.err != nil:
			outcome = "offline"
		case result.status == 304:
			outcome = "unchanged"
		default:
			moved := result.response.V != session.V
			session.V = result.response.V
			// Something new needs the person (a conflict opened, a thought
			// waits to be confirmed): say so on the line they see.
			if needs := result.response.NeedsYou; needs != nil {
				if *needs > session.NeedsYou {
					visible = needsYouLine(session, *needs)
				}
				session.NeedsYou = *needs
			}
			session.LastBriefAt = time.Now().UTC().Format(time.RFC3339)
			_ = state.SaveSession(session)
			if result.response.Text != nil {
				update = strings.TrimSpace(*result.response.Text)
			}
			if hold, ok := state.LoadHold(sessionID); ok {
				for _, resolved := range result.response.ResolvedHolds {
					if resolved.SessionID == sessionID || resolved.Ref == hold.Ref || resolved.Ref == hold.ClientRef {
						state.RemoveHold(sessionID)
					}
				}
			}
			if moved {
				spawnRefresh(h.agent, h.version, sessionID)
			}
		}
	}
	parts := []string{}
	if update != "" {
		parts = append(parts, update)
	}
	if hold, ok := state.LoadHold(sessionID); ok {
		parts = append(parts, HoldBlock(hold))
	}
	if len(parts) == 0 && visible == "" {
		return outcome
	}
	h.contextOutput("UserPromptSubmit", strings.Join(parts, "\n\n"), visible)
	return outcome
}

// needsYouLine is the visible line when Review grows mid-session.
func needsYouLine(session state.Session, needs int) string {
	name := session.ProjectName
	if name == "" {
		name = filepath.Base(firstNonEmpty(session.RepoRoot, session.CWD))
	}
	noun := "things need you"
	if needs == 1 {
		noun = "thing needs you"
	}
	return fmt.Sprintf("Recallstack · %s · %d %s · recallstack review", name, needs, noun)
}

// HoldBlock is what every prompt adds while a hold is open (spec section 9).
// A hold on a conflict is settled on the conflict itself (spec section 10).
func HoldBlock(hold state.Hold) string {
	ref := hold.DisplayRef()
	if holdOnConflict(hold) {
		return fmt.Sprintf(`<recallstack_hold ref="%[1]s" against="%[2]s">
You paused because your plan ("%[3]s") touches %[2]s, where two of %[4]s's rules disagree. %[4]s's message is probably the answer. Settle it before anything else:
recallstack resolve %[2]s a --quote "<the exact words>"
recallstack resolve %[2]s b --quote "<the exact words>"
recallstack resolve %[2]s neither --text "<what should hold instead>" --quote "<the exact words>"
recallstack resolve %[2]s later        (not now; it stays open and edits resume)
Don't recommend a side. If the answer is unclear, ask once: "Which holds: a, b, or neither?"
</recallstack_hold>`, ref, hold.AgainstRef, hold.Plan, hold.Person)
	}
	return fmt.Sprintf(`<recallstack_hold ref="%[1]s" against="%[2]s">
You paused because your plan ("%[3]s") goes against %[2]s. %[4]s's message is probably the answer. Resolve the hold before anything else:
recallstack resolve %[1]s change-plan
recallstack resolve %[1]s exception --text "<what is allowed, and where>" --quote "<the exact words>"
recallstack resolve %[1]s replace --text "<the new decision>" --quote "<the exact words>"
recallstack resolve %[1]s cancel        (they moved on; nothing is recorded)
If the answer is unclear, ask once: "Change the plan, or go ahead as an exception?"
</recallstack_hold>`, ref, hold.AgainstRef, hold.Plan, hold.Person)
}

// DenyReason is the before-tool deny text (spec section 9).
func DenyReason(hold state.Hold) string {
	if holdOnConflict(hold) {
		return fmt.Sprintf("Recallstack: paused on %s until %s answers. Ask, or settle it with recallstack resolve %s <a|b|neither>.", hold.AgainstRef, hold.Person, hold.AgainstRef)
	}
	return fmt.Sprintf("Recallstack: paused on %s until %s answers. Ask, or resolve it with recallstack resolve %s <choice>.", hold.AgainstRef, hold.Person, hold.DisplayRef())
}

func holdOnConflict(hold state.Hold) bool {
	if hold.AgainstType != "" {
		return hold.AgainstType == "conflict"
	}
	ref := hold.AgainstRef
	if index := strings.LastIndex(ref, ":"); index >= 0 {
		ref = ref[index+1:]
	}
	return strings.HasPrefix(strings.ToUpper(ref), "C")
}

func (h *hookRun) preTool() string {
	sessionID := h.sessionID()
	hold, holding := state.LoadHold(sessionID)
	if sessionID == "" || !holding {
		if h.agent == "cursor" {
			h.writeJSON(map[string]any{"permission": "allow"})
		}
		return "allow"
	}
	tool := firstNonEmpty(stringValue(h.payload["tool_name"]), stringValue(h.payload["toolName"]))
	input := map[string]any{}
	switch value := h.payload["tool_input"].(type) {
	case map[string]any:
		input = value
	case string:
		input = map[string]any{"command": value}
	}
	if args := stringValue(h.payload["toolArgs"]); args != "" {
		parsed := readPayload(strings.NewReader(args))
		if len(parsed) > 0 {
			input = parsed
		}
	}
	if h.agent == "cursor" {
		input = map[string]any{"command": stringValue(h.payload["command"])}
	}
	if !toolGated(h.agent, tool, input) {
		if h.agent == "cursor" {
			h.writeJSON(map[string]any{"permission": "allow"})
		}
		return "allow_while_holding"
	}
	reason := DenyReason(hold)
	switch {
	case h.agent == "cursor":
		h.writeJSON(map[string]any{"permission": "deny", "userMessage": reason, "agentMessage": reason})
	case h.agent == "copilot" && !h.vscodeShape:
		h.writeJSON(map[string]any{"permissionDecision": "deny", "permissionDecisionReason": reason})
	default:
		h.writeJSON(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason,
		}})
	}
	return "deny"
}

func (h *hookRun) sessionEnd(ctx context.Context) string {
	sessionID := h.sessionID()
	if sessionID == "" {
		return "no_session_id"
	}
	if h.agent == "copilot" {
		// Copilot captures at session end (today's worker).
		h.stop()
	}
	state.RemoveHold(sessionID)
	session, known := state.LoadSession(h.agent, sessionID)
	if known {
		if session.AgentPID > 0 {
			if link, ok := state.LoadPIDLink(session.AgentPID); ok && link.SessionID == sessionID {
				state.Remove(state.PIDPath(session.AgentPID))
			}
		}
		session.EndedAt = time.Now().UTC().Format(time.RFC3339)
		_ = state.SaveSession(session)
	}
	networkCtx, cancel := context.WithDeadline(ctx, h.started.Add(endNetworkBudget))
	defer cancel()
	done := make(chan string, 1)
	go func() {
		svc := remote.New(h.version)
		if !svc.SignedIn() {
			done <- "signed_out"
			return
		}
		var ignored any
		queued, err := svc.Send(networkCtx, remote.Write{
			Path: "/v1/sessions/" + url.PathEscape(sessionID) + "/end", Label: "session_end",
			IdempotencyKey: "session-end-" + h.agent + "-" + sessionID,
			Body:           map[string]any{"agent": h.agent, "ended_at": time.Now().UTC().Format(time.RFC3339)},
		}, &ignored)
		switch {
		case err != nil:
			done <- "error"
		case queued:
			done <- "queued"
		default:
			done <- "ok"
		}
	}()
	select {
	case outcome := <-done:
		return outcome
	case <-networkCtx.Done():
		// The request was cut off; give the queue write a moment to land.
		select {
		case outcome := <-done:
			return outcome
		case <-time.After(200 * time.Millisecond):
			return "timeout"
		}
	}
}
