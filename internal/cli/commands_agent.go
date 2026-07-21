package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/o-clan/recallstack-cli/internal/integrations"
	"github.com/o-clan/recallstack-cli/internal/target"
)

func (r *runtimeServices) runAgent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("agent requires model, install, uninstall, or status")
	}
	switch args[0] {
	case "model":
		return r.runAgentModel(args[1:])
	case "install":
		return r.runAgentInstall(args[1:])
	case "uninstall":
		return r.runAgentUninstall(args[1:])
	case "status":
		return r.runAgentStatus(args[1:])
	default:
		return fmt.Errorf("unknown agent command %q", args[0])
	}
}

func (r *runtimeServices) integrationManager(cwd string) (*integrations.Manager, error) {
	manager, err := integrations.New(cwd)
	if err != nil {
		return nil, err
	}
	manager.Version = r.deps.Version
	return manager, nil
}

func (r *runtimeServices) runAgentModel(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"cwd"}, []string{"global", "clear"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) > 2 {
		return fmt.Errorf("agent model accepts a target and model")
	}
	cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
	manager, err := r.integrationManager(cwd)
	if err != nil {
		return err
	}
	scope := choose(parsed.bools["global"], integrations.ScopeGlobal, integrations.ScopeLocal)
	if len(parsed.positionals) == 0 {
		if parsed.bools["clear"] {
			return fmt.Errorf("choose a target before setting or clearing a model")
		}
		out, err := manager.ListWorkerModels(scope)
		if err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, out)
	}
	targetName, err := integrations.ParseTarget(parsed.positionals[0])
	if err != nil || targetName == integrations.TargetAll || targetName == integrations.TargetCursor || targetName == integrations.TargetAntigravity {
		return fmt.Errorf("invalid target. Use codex, claude, or copilot")
	}
	model := ""
	if len(parsed.positionals) == 2 {
		model = strings.TrimSpace(parsed.positionals[1])
	}
	if parsed.bools["clear"] && model != "" {
		return fmt.Errorf("provide either a model or --clear, not both")
	}
	if parsed.bools["clear"] {
		cleared, err := manager.ClearWorkerModel(targetName, scope)
		if err != nil {
			return err
		}
		status, err := manager.WorkerModel(targetName, scope)
		if err != nil {
			return err
		}
		path := any(nil)
		if cleared != nil {
			path = cleared["path"]
		}
		return writeJSON(r.deps.Stdout, map[string]any{"scope": scope, "target": targetName, "cleared": cleared != nil, "agent_settings_path": path, "model": status})
	}
	if model == "" {
		status, err := manager.WorkerModel(targetName, scope)
		if err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, status)
	}
	applied, err := manager.SetWorkerModel(targetName, model, scope)
	if err != nil {
		return err
	}
	status, err := manager.WorkerModel(targetName, scope)
	if err != nil {
		return err
	}
	return writeJSON(r.deps.Stdout, map[string]any{
		"scope": scope, "target": targetName, "worker_model": model,
		"agent_settings_path": applied["path"], "model": status,
	})
}

func (r *runtimeServices) runAgentInstall(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"cwd", "project", "worker-model"}, []string{"global", "no-prompt"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 {
		return fmt.Errorf("agent install requires one target")
	}
	targetName, err := integrations.ParseTarget(parsed.positionals[0])
	if err != nil {
		return err
	}
	cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
	scope := choose(parsed.bools["global"], integrations.ScopeGlobal, integrations.ScopeLocal)
	if scope == integrations.ScopeGlobal && parsed.values["project"] != "" {
		return fmt.Errorf("`--project` cannot be used with `agent install --global`. Configure repo targets separately with `recallstack project use`")
	}
	manager, err := r.integrationManager(cwd)
	if err != nil {
		return err
	}
	var selected *target.Resolved
	var previous *target.Resolved
	var workspacePath any
	if scope == integrations.ScopeLocal {
		previous, _ = r.targets.LoadLocal(context.Background(), cwd)
		if parsed.values["project"] != "" {
			value, err := r.targets.ResolveForCommand(context.Background(), cwd, parsed.values["project"], true)
			if err != nil {
				return err
			}
			selected = &value
		} else if previous != nil {
			selected = previous
		} else if parsed.bools["no-prompt"] {
			return fmt.Errorf("no default project configured for this directory. Run `recallstack project use` first")
		} else {
			choices, err := r.writableTargets(context.Background(), false)
			if err != nil {
				return err
			}
			if len(choices) == 0 {
				return fmt.Errorf("no writable projects found across accessible workspaces")
			}
			for index, item := range choices {
				fmt.Fprintf(r.deps.Stdout, " [%d] %s (%s)\n", index+1, item.Canonical(), item.ProjectName)
			}
			answer, err := promptLine(r.deps.Stdin, r.deps.Stdout, "Choose project [1]: ")
			if err != nil {
				return err
			}
			selected = &choices[0]
			for index := range choices {
				if answer == fmt.Sprint(index+1) || answer == choices[index].Canonical() {
					selected = &choices[index]
				}
			}
		}
		path, err := r.targets.Save(cwd, *selected)
		if err != nil {
			return err
		}
		workspacePath = path
	}
	installed, err := manager.Install(targetName, integrations.InstallOptions{Scope: scope, WorkerModel: parsed.values["worker-model"]})
	if err != nil {
		return err
	}
	response := map[string]any{
		"cwd": cwd, "scope": scope, "workspace_config_path": workspacePath, "target": nil,
		"workspace_id": nil, "workspace_slug": nil, "project_id": nil, "project_slug": nil,
		"previous_target": nil, "agent_settings_path": nil, "installed": installed,
	}
	if parsed.values["worker-model"] != "" && targetName != integrations.TargetCursor && targetName != integrations.TargetAntigravity {
		response["agent_settings_path"] = manager.AgentSettingsPath(scope)
	}
	if selected != nil {
		response["target"] = selected.Canonical()
		response["workspace_id"], response["workspace_slug"] = selected.WorkspaceID, selected.WorkspaceSlug
		response["project_id"], response["project_slug"] = selected.ProjectID, selected.ProjectSlug
	}
	if previous != nil {
		response["previous_target"] = previous.Canonical()
	}
	return writeJSON(r.deps.Stdout, response)
}

func (r *runtimeServices) runAgentUninstall(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"cwd"}, []string{"global", "purge-project-state"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 {
		return fmt.Errorf("agent uninstall requires one target")
	}
	targetName, err := integrations.ParseTarget(parsed.positionals[0])
	if err != nil {
		return err
	}
	cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
	scope := choose(parsed.bools["global"], integrations.ScopeGlobal, integrations.ScopeLocal)
	if scope == integrations.ScopeGlobal && parsed.bools["purge-project-state"] {
		return fmt.Errorf("`--purge-project-state` is only valid for local uninstall")
	}
	manager, err := r.integrationManager(cwd)
	if err != nil {
		return err
	}
	agentSettingsPath := any(nil)
	if _, statErr := os.Stat(manager.AgentSettingsPath(scope)); statErr == nil {
		agentSettingsPath = manager.AgentSettingsPath(scope)
	}
	removed, err := manager.Uninstall(targetName, integrations.UninstallOptions{Scope: scope, PurgeProjectState: parsed.bools["purge-project-state"]})
	if err != nil {
		return err
	}
	return writeJSON(r.deps.Stdout, map[string]any{
		"cwd": cwd, "scope": scope, "target": targetName,
		"purge_project_state": parsed.bools["purge-project-state"], "agent_settings_path": agentSettingsPath, "removed": removed,
	})
}

func (r *runtimeServices) runAgentStatus(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec([]string{"cwd"}, []string{"global"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 0 {
		return fmt.Errorf("agent status does not accept positional arguments")
	}
	cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
	scope := choose(parsed.bools["global"], integrations.ScopeGlobal, integrations.ScopeLocal)
	manager, err := r.integrationManager(cwd)
	if err != nil {
		return err
	}
	status, err := manager.Status(scope)
	if err != nil {
		return err
	}
	for _, targetName := range []integrations.Target{integrations.TargetCodex, integrations.TargetClaude, integrations.TargetCopilot} {
		section, ok := status[string(targetName)].(map[string]any)
		if !ok {
			continue
		}
		model, err := manager.WorkerModel(targetName, scope)
		if err != nil {
			return err
		}
		section["worker_model"] = model.WorkerModel
		section["worker_model_source"] = model.WorkerModelSource
		section["worker_model_change_command"] = model.ChangeCommand
		section["worker_model_clear_command"] = model.ClearCommand
	}
	status["config_path"] = r.store.Resolve(r.cwd).Path
	status["api_base"] = r.config.EffectiveBaseURL
	status["authenticated"] = r.config.AccessToken != "" || r.config.APIKey != ""
	status["auth_status"] = r.authStatus()
	status["workspace_config_path"] = nil
	status["target"], status["workspace_id"], status["workspace_slug"], status["project_id"], status["project_slug"] = nil, nil, nil, nil, nil
	if scope == integrations.ScopeLocal {
		path, _, _ := target.FindWorkspaceConfig(cwd)
		if path != "" {
			status["workspace_config_path"] = path
		}
		selected, _ := r.targets.LoadLocal(context.Background(), cwd)
		if selected != nil {
			status["target"] = selected.Canonical()
			status["workspace_id"], status["workspace_slug"] = selected.WorkspaceID, selected.WorkspaceSlug
			status["project_id"], status["project_slug"] = selected.ProjectID, selected.ProjectSlug
		}
	}
	return writeJSON(r.deps.Stdout, status)
}
