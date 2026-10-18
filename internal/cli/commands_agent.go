package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/integrations"
	"github.com/o-clan/recallstack-cli/internal/project"
	"github.com/o-clan/recallstack-cli/internal/state"
)

func (r *runtimeServices) runAgent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: recallstack agent <install|status|uninstall|model>")
	}
	switch args[0] {
	case "install":
		return r.runAgentInstall(args[1:])
	case "status":
		return r.runAgentStatus(args[1:])
	case "uninstall":
		return r.runAgentUninstall(args[1:])
	case "model":
		return r.runAgentModel(args[1:])
	}
	return fmt.Errorf("unknown agent command %q", args[0])
}

func (r *runtimeServices) integrationManager(scope integrations.Scope) (*integrations.Manager, error) {
	root := r.cwd
	if scope == integrations.ScopeLocal {
		if folder := project.Detect(r.cwd); folder.RepoRoot != "" {
			root = folder.RepoRoot
		}
	}
	manager, err := integrations.New(root)
	if err != nil {
		return nil, err
	}
	manager.Version = r.deps.Version
	return manager, nil
}

const nameColumn = 15

func padName(name string) string {
	if len([]rune(name)) >= nameColumn {
		return name + " "
	}
	return name + strings.Repeat(" ", nameColumn-len([]rune(name)))
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

func (r *runtimeServices) runAgentInstall(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"repo", "no-import", "json", "global"}))
	if err != nil {
		return err
	}
	scope := integrations.ScopeGlobal
	if parsed.bools["repo"] {
		scope = integrations.ScopeLocal
	}
	manager, err := r.integrationManager(scope)
	if err != nil {
		return err
	}
	selected := []integrations.Target{}
	explicit := len(parsed.positionals) > 0
	for _, name := range parsed.positionals {
		target, err := integrations.ParseTarget(name)
		if err != nil {
			return err
		}
		if target == integrations.TargetAll {
			selected = append(selected, integrations.AllTargets...)
			continue
		}
		selected = append(selected, target)
	}
	results := []integrations.Result{}
	toInstall := []integrations.Target{}
	if explicit {
		toInstall = integrations.SortTargets(selected)
	} else {
		for _, target := range integrations.AllTargets {
			if manager.Found(target) {
				toInstall = append(toInstall, target)
			}
		}
	}
	installed := manager.Install(toInstall, scope)
	byTarget := map[integrations.Target]integrations.Result{}
	for _, result := range installed {
		byTarget[result.Target] = result
	}
	if explicit {
		results = installed
	} else {
		for _, target := range integrations.AllTargets {
			if result, ok := byTarget[target]; ok {
				results = append(results, result)
				continue
			}
			results = append(results, integrations.Result{Target: target, Name: integrations.DisplayNames[target], Status: "not_found", Fix: []string{}})
		}
	}
	found, connected := 0, 0
	connectedNames := []string{}
	waitingNames := []string{}
	for _, result := range results {
		if result.Status != "not_found" {
			found++
		}
		if result.Status == "connected" || result.Status == "waiting_for_trust" {
			connected++
			connectedNames = append(connectedNames, result.Name)
		}
		if result.Status == "waiting_for_trust" {
			waitingNames = append(waitingNames, result.Name)
		}
	}
	exitCode := 0
	switch {
	case found == 0 || connected == 0:
		exitCode = 1
	case connected < found:
		exitCode = 2
	}

	ctx := context.Background()
	folder := project.Detect(r.cwd)
	entry, known := project.Lookup(folder.Key)
	signedIn := r.svc.SignedIn()
	if !known && folder.InRepo && signedIn {
		resolveCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		resolved, _, err := project.Resolve(resolveCtx, r.svc, folder)
		cancel()
		if err == nil && resolved.ProjectID != "" {
			entry, known = resolved, true
		}
	}
	projectName := folder.Name
	if known {
		projectName = entry.Name
	}

	if parsed.bools["json"] {
		agents := map[string]any{}
		for _, result := range results {
			agents[string(result.Target)] = result
		}
		var projectValue any
		if folder.InRepo || known {
			projectValue = map[string]any{"name": projectName, "id": nullable(entry.ProjectID), "key": folder.Key, "recognised_from": choose(folder.InRepo, "git repo", "project use")}
		}
		if err := writeJSON(r.deps.Stdout, map[string]any{
			"device_name": deviceName(), "scope": choose(scope == integrations.ScopeGlobal, "user", "repo"),
			"installed": agents, "project": projectValue, "signed_in": signedIn, "exit_code": exitCode,
		}); err != nil {
			return err
		}
		if files := r.ruleFilesForOffer(); len(files) > 0 && !state.Exists(state.ImportOfferedPath()) {
			fmt.Fprintf(r.deps.Stderr, "Found rules you wrote for your agents in %d files. Turn them into thoughts:  recallstack import\n", len(files))
		}
		if exitCode != 0 {
			return &ExitError{Code: exitCode}
		}
		return nil
	}

	out := r.deps.Stdout
	fmt.Fprintf(out, "Looking for agents on %s\n\n", deviceName())
	for _, result := range results {
		switch result.Status {
		case "connected":
			fmt.Fprintf(out, "  ✓ %s%s\n", padName(result.Name), result.Summary)
			if result.Note != "" {
				fmt.Fprintf(out, "    %s%s\n", strings.Repeat(" ", nameColumn), result.Note)
			}
		case "waiting_for_trust":
			fmt.Fprintf(out, "  ! %s%s\n", padName(result.Name), result.Summary)
			for _, line := range result.Fix {
				fmt.Fprintf(out, "    %s%s\n", strings.Repeat(" ", nameColumn), line)
			}
		case "failed":
			fmt.Fprintf(out, "  ✗ %s%s\n", padName(result.Name), result.Summary)
			for _, line := range result.Fix {
				fmt.Fprintf(out, "    %s%s\n", strings.Repeat(" ", nameColumn), line)
			}
		default:
			fmt.Fprintf(out, "  · %snot found\n", padName(result.Name))
		}
	}
	fmt.Fprintln(out)
	if found == 0 {
		fmt.Fprintln(out, "No agents found on this computer. Install Claude Code, Codex, Cursor, Copilot or Antigravity, then run recallstack agent install again.")
		return &ExitError{Code: 1}
	}
	switch {
	case folder.InRepo:
		fmt.Fprintf(out, "Project  %s, recognised from this git repo\n", projectName)
		fmt.Fprintln(out, "         Rename it with  recallstack project rename <name>")
	case known:
		fmt.Fprintf(out, "Project  %s, named for this folder\n", projectName)
	default:
		fmt.Fprintln(out, "Project  not named yet, this folder isn't a git repo")
		fmt.Fprintln(out, "         Name it with  recallstack project use \"<name>\"")
	}
	fmt.Fprintln(out)
	where := "for every repo on this computer"
	if scope == integrations.ScopeLocal {
		where = "for this repo only"
	}
	if connected < found {
		fmt.Fprintf(out, "Connected %d of %d agents, %s.\n", connected, found, where)
	} else {
		switch {
		case len(waitingNames) > 0 && connected == 1:
			fmt.Fprintf(out, "%s installed, %s. It starts once you trust its hooks in /hooks.\n", connectedNames[0], where)
		case len(waitingNames) > 0:
			fmt.Fprintf(out, "All %d agents installed. %s starts once you trust its hooks in /hooks.\n", connected, joinNames(waitingNames))
		case connected == 1:
			fmt.Fprintf(out, "%s connected, %s.\n", connectedNames[0], where)
		default:
			fmt.Fprintf(out, "All %d agents connected.\n", connected)
		}
		if !parsed.bools["no-import"] && r.interactive() && !state.Exists(state.ImportOfferedPath()) {
			if files := r.ruleFilesForOffer(); len(files) > 0 {
				fmt.Fprintln(out)
				if err := r.importAtInstall(); err != nil {
					if _, message := AsExit(err); message != "" {
						fmt.Fprintln(out, message)
					}
				}
			}
		} else if !parsed.bools["no-import"] && !state.Exists(state.ImportOfferedPath()) {
			if files := r.ruleFilesForOffer(); len(files) > 0 {
				fmt.Fprintf(out, "Found rules you wrote for your agents in %d files. Turn them into thoughts:  recallstack import\n", len(files))
			}
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Next")
		for _, name := range waitingNames {
			fmt.Fprintf(out, "  Open %s, run /hooks and trust the Recallstack hooks. Until then it skips them.\n", name)
		}
		fmt.Fprintf(out, "  Start a new session in %s. It opens with a short brief.\n", joinNames(connectedNames))
		fmt.Fprintln(out, "  Sessions that are already open won't get it until you restart them.")
		fmt.Fprintln(out, "  See what they'll be told:  recallstack brief")
		fmt.Fprintln(out, "  Installed another agent later? Run recallstack agent install again.")
	}
	if !signedIn {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Not signed in yet. Run recallstack login so the brief can load.")
	}
	if exitCode != 0 {
		return &ExitError{Code: exitCode}
	}
	return nil
}

func (r *runtimeServices) interactive() bool {
	in, ok := r.deps.Stdin.(*os.File)
	if !ok {
		return false
	}
	out, ok := r.deps.Stdout.(*os.File)
	if !ok {
		return false
	}
	inInfo, err1 := in.Stat()
	outInfo, err2 := out.Stat()
	return err1 == nil && err2 == nil && inInfo.Mode()&os.ModeCharDevice != 0 && outInfo.Mode()&os.ModeCharDevice != 0
}

func (r *runtimeServices) runAgentStatus(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"json", "global"}))
	if err != nil {
		return err
	}
	global, err := r.integrationManager(integrations.ScopeGlobal)
	if err != nil {
		return err
	}
	local, err := r.integrationManager(integrations.ScopeLocal)
	if err != nil {
		return err
	}
	folder := project.Detect(r.cwd)
	entry, known := project.Lookup(folder.Key)
	signedIn := r.svc.SignedIn()
	me := state.LoadMe()
	cfg, _ := r.svc.Config()
	waiting := len(r.svc.Queue().List(cfg.EffectiveBaseURL))
	type row struct {
		result   integrations.Result
		where    string
		capture  time.Time
		delivery string
		trust    *integrations.CodexTrust
	}
	rows := []row{}
	for _, result := range global.Detect(integrations.ScopeGlobal) {
		where := "user"
		if !result.Connected && folder.InRepo {
			for _, repo := range local.Detect(integrations.ScopeLocal) {
				if repo.Target == result.Target && repo.Connected {
					result.Connected, result.Status, where = true, "connected", "repo"
				}
			}
		}
		item := row{result: result, where: where, capture: integrations.LastCapture(result.Target), delivery: integrations.BriefDelivery(result.Target)}
		if result.Target == integrations.TargetCodex && result.Connected {
			manager := global
			scope := integrations.ScopeGlobal
			if where == "repo" {
				manager, scope = local, integrations.ScopeLocal
			}
			if trust, ok := manager.CodexHooksTrust(scope); ok {
				item.trust = &trust
			}
		}
		rows = append(rows, item)
	}
	if parsed.bools["json"] {
		agents := map[string]any{}
		for _, item := range rows {
			var last any
			if !item.capture.IsZero() {
				last = item.capture.UTC().Format(time.RFC3339)
			}
			status := item.result.Status
			if item.trust != nil && !item.trust.Ready() {
				status = "waiting_for_trust"
			}
			entry := map[string]any{
				"found": item.result.Found, "hook_installed": item.result.Connected, "status": status,
				"scope": choose(item.result.Connected, item.where, ""), "brief_delivery": item.delivery, "last_capture_at": last,
				"worker_model": nullable(integrations.WorkerModel(item.result.Target)),
			}
			if item.trust != nil {
				entry["codex_trust"] = item.trust
			}
			agents[string(item.result.Target)] = entry
		}
		var projectValue any
		if known {
			projectValue = map[string]any{"id": entry.ProjectID, "name": entry.Name, "key": folder.Key}
		}
		return writeJSON(r.deps.Stdout, map[string]any{
			"signed_in": signedIn, "email": nullable(me.Email), "device_name": deviceName(), "api_base": cfg.EffectiveBaseURL,
			"auth_status": r.authStatus(), "pending_writes": waiting, "project": projectValue, "agents": agents,
			"state_root": state.Root(),
		})
	}
	out := r.deps.Stdout
	if signedIn && me.Email != "" {
		fmt.Fprintf(out, "Signed in as %s on %s\n", me.Email, deviceName())
	} else if signedIn {
		fmt.Fprintf(out, "Signed in on %s\n", deviceName())
	} else {
		fmt.Fprintln(out, "Not signed in. Run recallstack login.")
	}
	switch {
	case known && folder.InRepo:
		fmt.Fprintf(out, "Project   %s (this repo)\n", entry.Name)
	case known:
		fmt.Fprintf(out, "Project   %s (this folder)\n", entry.Name)
	case folder.InRepo:
		fmt.Fprintf(out, "Project   %s (this repo, recognised at the next session)\n", folder.Name)
	default:
		fmt.Fprintln(out, "Project   not named yet")
	}
	fmt.Fprintln(out)
	for _, item := range rows {
		switch {
		case item.trust != nil && !item.trust.Ready():
			fmt.Fprintf(out, "  ! %s%s\n", padName(item.result.Name), codexWaitingStatus(*item.trust, item.where == "repo"))
		case item.result.Connected:
			parts := []string{"connected"}
			if item.where == "repo" {
				parts[0] = "connected in this repo"
			}
			parts = append(parts, item.delivery)
			if !item.capture.IsZero() {
				parts = append(parts, "last capture "+shortTime(item.capture))
			}
			fmt.Fprintf(out, "  ✓ %s%s\n", padName(item.result.Name), strings.Join(parts, " · "))
		case item.result.Found:
			fmt.Fprintf(out, "  · %sfound, not connected · recallstack agent install %s\n", padName(item.result.Name), item.result.Target)
		default:
			fmt.Fprintf(out, "  · %snot found\n", padName(item.result.Name))
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Waiting to send: %d\n", waiting)
	return nil
}

// codexWaitingStatus is the status line for Codex hooks that Codex won't run yet.
func codexWaitingStatus(trust integrations.CodexTrust, repo bool) string {
	switch {
	case !trust.RepoTrusted:
		return "installed in this repo, waiting for you to trust this folder in Codex, then the hooks in /hooks"
	case trust.Waiting == 0 && trust.Disabled > 0:
		return "installed, turned off in Codex /hooks"
	}
	for _, hook := range trust.Hooks {
		if hook.Status == "modified" {
			return "installed, changed since you trusted them, waiting for you to trust them again in Codex /hooks"
		}
	}
	if repo {
		return "installed in this repo, waiting for you to trust them in Codex /hooks"
	}
	return "installed, waiting for you to trust them in Codex /hooks"
}

// shortTime is HH:MM today, else "Oct 10".
func shortTime(at time.Time) string {
	local := at.Local()
	now := time.Now()
	if local.Format("2006-01-02") == now.Format("2006-01-02") {
		return local.Format("15:04")
	}
	if local.Year() != now.Year() {
		return local.Format("Jan 2, 2006")
	}
	return local.Format("Jan 2")
}

func (r *runtimeServices) runAgentUninstall(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"repo", "global", "json"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 {
		return fmt.Errorf("usage: recallstack agent uninstall <codex|claude|cursor|copilot|antigravity|all> [--repo]")
	}
	target, err := integrations.ParseTarget(parsed.positionals[0])
	if err != nil {
		return err
	}
	scope := choose(parsed.bools["repo"], integrations.ScopeLocal, integrations.ScopeGlobal)
	manager, err := r.integrationManager(scope)
	if err != nil {
		return err
	}
	targets := []integrations.Target{target}
	if target == integrations.TargetAll {
		targets = integrations.AllTargets
	}
	results := map[string]any{}
	failed := false
	for _, item := range targets {
		if target == integrations.TargetAll && !manager.Connected(item, scope) {
			continue
		}
		removed, err := manager.Uninstall(item, scope)
		if err != nil {
			failed = true
			results[string(item)] = map[string]any{"error": err.Error()}
			if !parsed.bools["json"] {
				fmt.Fprintf(r.deps.Stdout, "  ✗ %s%s\n", padName(integrations.DisplayNames[item]), err.Error())
			}
			continue
		}
		results[string(item)] = removed
		if !parsed.bools["json"] {
			fmt.Fprintf(r.deps.Stdout, "  · %sdisconnected\n", padName(integrations.DisplayNames[item]))
		}
	}
	if parsed.bools["json"] {
		if err := writeJSON(r.deps.Stdout, map[string]any{"scope": scope, "removed": results}); err != nil {
			return err
		}
	}
	if failed {
		return &ExitError{Code: 2}
	}
	return nil
}

func (r *runtimeServices) runAgentModel(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"clear", "global", "json"}))
	if err != nil {
		return err
	}
	out := r.deps.Stdout
	if len(parsed.positionals) == 0 {
		if parsed.bools["clear"] {
			return fmt.Errorf("choose an agent before clearing its model")
		}
		models := map[string]any{}
		for _, target := range integrations.ModelTargets() {
			models[string(target)] = nullable(integrations.WorkerModel(target))
			if !parsed.bools["json"] {
				model := integrations.WorkerModel(target)
				if model == "" {
					model = "the agent's default"
				}
				fmt.Fprintf(out, "  %s%s\n", padName(integrations.DisplayNames[target]), model)
			}
		}
		if parsed.bools["json"] {
			return writeJSON(out, map[string]any{"models": models, "settings_path": state.AgentSettingsPath()})
		}
		return nil
	}
	target, err := integrations.ParseTarget(parsed.positionals[0])
	if err != nil || (target != integrations.TargetCodex && target != integrations.TargetClaude && target != integrations.TargetCopilot) {
		return fmt.Errorf("models can be set for codex, claude and copilot")
	}
	name := integrations.DisplayNames[target]
	switch {
	case parsed.bools["clear"]:
		if _, err := integrations.ClearWorkerModel(target); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s reads turns with its default model.\n", name)
	case len(parsed.positionals) == 2:
		if err := integrations.SetWorkerModel(target, parsed.positionals[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s reads turns with %s.\n", name, strings.TrimSpace(parsed.positionals[1]))
	default:
		model := integrations.WorkerModel(target)
		if model == "" {
			model = "its default model"
		}
		fmt.Fprintf(out, "%s reads turns with %s.\n", name, model)
	}
	return nil
}
