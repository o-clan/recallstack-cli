package integrations

import (
	"fmt"
	"path/filepath"
)

func (m *Manager) Uninstall(target Target, options UninstallOptions) (map[string]any, error) {
	if options.Scope == "" {
		options.Scope = ScopeLocal
	}
	if err := m.normalize(options.Scope); err != nil {
		return nil, err
	}
	if !target.Valid() {
		return nil, fmt.Errorf("invalid target %q", target)
	}
	if options.Scope == ScopeGlobal && options.PurgeProjectState {
		return nil, fmt.Errorf("purge project state is only valid for local uninstall")
	}
	out := map[string]any{}
	for _, item := range targets(target) {
		var result object
		var err error
		switch item {
		case TargetCodex:
			result, err = m.uninstallCodex(options.Scope)
		case TargetClaude:
			result, err = m.uninstallClaude(options.Scope)
		case TargetCursor:
			result, err = m.uninstallCursor(options.Scope)
		case TargetCopilot:
			result, err = m.uninstallCopilot(options.Scope)
		case TargetAntigravity:
			result, err = m.uninstallAntigravity(options.Scope)
		}
		if err != nil {
			return nil, err
		}
		out[string(item)] = result
	}
	if options.PurgeProjectState {
		state, err := m.purgeProjectState()
		if err != nil {
			return nil, err
		}
		out["project_state"] = state
	}
	if _, err := m.ClearWorkerModel(target, options.Scope); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Manager) purgeProjectState() (object, error) {
	root := filepath.Join(m.CWD, ".recallstack")
	workspacePath := filepath.Join(root, "workspace.json")
	projectPath := filepath.Join(root, "config.json")
	turnsPath := filepath.Join(root, "turns")
	agentTurnsPath := filepath.Join(root, "agent-turns")
	workspaceRemoved, err := remove(workspacePath)
	if err != nil {
		return nil, err
	}
	projectRemoved, err := remove(projectPath)
	if err != nil {
		return nil, err
	}
	turnsRemoved, err := remove(turnsPath)
	if err != nil {
		return nil, err
	}
	agentTurnsRemoved, err := remove(agentTurnsPath)
	if err != nil {
		return nil, err
	}
	rootRemoved, err := remove(root)
	if err != nil {
		return nil, err
	}
	return object{
		"project_state_root": root, "project_state_root_removed": rootRemoved,
		"workspace_config_path": workspacePath, "workspace_config_removed": workspaceRemoved,
		"project_config_path": projectPath, "project_config_removed": projectRemoved,
		"turns_path": turnsPath, "turns_removed": turnsRemoved,
		"agent_turns_path": agentTurnsPath, "agent_turns_removed": agentTurnsRemoved,
	}, nil
}

func (m *Manager) uninstallCodex(scope Scope) (object, error) {
	paths := m.codexPaths(scope)
	skillRemoved, err := remove(paths.skillDir)
	if err != nil {
		return nil, err
	}
	legacyRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	configUpdated, configRemoved, err := removeNestedHookCommands(paths.hooksConfigPath, "hooks", []string{"SessionStart", "Stop"}, "command", commandBelongsTo(TargetCodex))
	if err != nil {
		return nil, err
	}
	tomlUpdated, tomlRemoved := false, false
	if exists(paths.configPath) {
		current, err := readText(paths.configPath)
		if err != nil {
			return nil, err
		}
		next := removeTomlKey(current, "features", "codex_hooks")
		tomlUpdated = next != current
		if tomlUpdated && next == "" {
			tomlRemoved, err = remove(paths.configPath)
		} else if tomlUpdated {
			err = writeFile(paths.configPath, []byte(next), 0o644)
		}
		if err != nil {
			return nil, err
		}
	}
	pruneEmpty([]string{filepath.Dir(paths.legacyHookPath), filepath.Dir(paths.skillDir), paths.root}, scopeRoot(m, scope))
	return object{
		"scope": scope, "skill_dir": paths.skillDir, "skill_path": paths.skillPath, "skill_removed": skillRemoved,
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"hooks_config_path": paths.hooksConfigPath, "hooks_config_updated": configUpdated, "hooks_config_removed": configRemoved,
		"config_path": paths.configPath, "config_updated": tomlUpdated, "config_removed": tomlRemoved,
	}, nil
}

func (m *Manager) uninstallClaude(scope Scope) (object, error) {
	paths := m.claudePaths(scope)
	mjsRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	shellRemoved, err := remove(paths.legacyShellPath)
	if err != nil {
		return nil, err
	}
	skillRemoved, err := remove(paths.skillPath)
	if err != nil {
		return nil, err
	}
	updated, removed, err := removeNestedHookCommands(paths.settingsPath, "hooks", []string{"UserPromptSubmit", "Stop"}, "command", commandBelongsTo(TargetClaude))
	if err != nil {
		return nil, err
	}
	pruneEmpty([]string{filepath.Dir(paths.legacyHookPath), filepath.Dir(paths.skillPath), paths.root}, scopeRoot(m, scope))
	return object{
		"scope": scope, "skill_path": paths.skillPath, "skill_removed": skillRemoved,
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": mjsRemoved || shellRemoved,
		"settings_path": paths.settingsPath, "settings_updated": updated, "settings_removed": removed,
	}, nil
}

func (m *Manager) uninstallCursor(scope Scope) (object, error) {
	paths := m.cursorPaths(scope)
	ruleRemoved, err := remove(paths.rulePath)
	if err != nil {
		return nil, err
	}
	legacyRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	updated, removed, err := removeNestedHookCommands(paths.hooksConfigPath, "hooks", []string{"beforeSubmitPrompt", "afterAgentResponse", "stop"}, "command", commandBelongsTo(TargetCursor))
	if err != nil {
		return nil, err
	}
	pruneEmpty([]string{filepath.Dir(paths.rulePath), filepath.Dir(paths.legacyHookPath), paths.root}, scopeRoot(m, scope))
	return object{
		"scope": scope, "rule_path": paths.rulePath, "rule_removed": ruleRemoved,
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"hooks_config_path": paths.hooksConfigPath, "hooks_config_updated": updated, "hooks_config_removed": removed,
		"ide_only": true,
	}, nil
}

func (m *Manager) uninstallAntigravity(scope Scope) (object, error) {
	paths := m.antigravityPaths(scope)
	if scope == ScopeGlobal {
		removed, err := remove(paths.root)
		return object{"scope": scope, "plugin_root": paths.root, "plugin_root_removed": removed, "cli_plugin": true}, err
	}
	skillRemoved, err := remove(paths.skillDir)
	if err != nil {
		return nil, err
	}
	legacyRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	updated, configRemoved := false, false
	if exists(paths.hooksConfigPath) {
		config, err := readJSON(paths.hooksConfigPath)
		if err != nil {
			return nil, err
		}
		plugin := asObject(config["recallstack"])
		entries, changed := removeCommands(asSlice(plugin["Stop"]), commandBelongsTo(TargetAntigravity), "command")
		if changed {
			updated = true
			if len(entries) == 0 {
				delete(plugin, "Stop")
				delete(plugin, "enabled")
			} else {
				plugin["Stop"] = entries
			}
			if len(plugin) == 0 {
				delete(config, "recallstack")
			} else {
				config["recallstack"] = plugin
			}
			if len(config) == 0 {
				configRemoved, err = remove(paths.hooksConfigPath)
			} else {
				err = writeJSON(paths.hooksConfigPath, config)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	pruneEmpty([]string{filepath.Dir(paths.legacyHookPath), filepath.Dir(paths.skillDir), paths.root}, m.CWD)
	return object{
		"scope": scope, "root": paths.root, "skill_dir": paths.skillDir, "skill_removed": skillRemoved,
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"hooks_config_path": paths.hooksConfigPath, "hooks_config_updated": updated, "hooks_config_removed": configRemoved,
		"cli_plugin": false,
	}, nil
}

func (m *Manager) uninstallCopilot(scope Scope) (object, error) {
	paths := m.copilotPaths(scope)
	if scope == ScopeGlobal {
		result, _ := m.Run("copilot", "plugin", "uninstall", "recallstack", "--config-dir", paths.configDir)
		removed, err := remove(paths.pluginSourceRoot)
		return object{
			"scope": scope, "plugin_source_root": paths.pluginSourceRoot, "plugin_source_removed": removed,
			"installed_plugin_path": paths.installedPluginPath, "uninstall_status": result.Status,
			"uninstall_stdout": trimOutput(result.Stdout, 400), "uninstall_stderr": trimOutput(result.Stderr, 400),
			"cli_supported": true, "vscode_supported": true,
		}, err
	}
	instructionsRemoved := false
	if exists(paths.instructionsPath) {
		current, err := readText(paths.instructionsPath)
		if err != nil {
			return nil, err
		}
		next, changed := removeManagedInstructions(current)
		if changed {
			instructionsRemoved = true
			if next == "" {
				if _, err := remove(paths.instructionsPath); err != nil {
					return nil, err
				}
			} else if err := writeFile(paths.instructionsPath, []byte(next), 0o644); err != nil {
				return nil, err
			}
		}
	}
	legacyRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	cliUpdated, cliRemoved, err := removeNestedHookCommands(paths.cliHooksConfigPath, "hooks", []string{"sessionStart", "userPromptSubmitted", "sessionEnd"}, "bash", commandBelongsTo(TargetCopilot))
	if err != nil {
		return nil, err
	}
	vsUpdated, vsRemoved, err := removeNestedHookCommands(paths.vscodeHooksConfigPath, "hooks", []string{"SessionStart", "UserPromptSubmit", "Stop"}, "command", commandBelongsTo(TargetCopilot))
	if err != nil {
		return nil, err
	}
	pruneEmpty([]string{filepath.Dir(paths.legacyHookPath), filepath.Dir(paths.instructionsPath)}, m.CWD)
	return object{
		"scope": scope, "instructions_path": paths.instructionsPath, "instructions_removed": instructionsRemoved,
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"cli_hooks_config_path": paths.cliHooksConfigPath, "cli_hooks_updated": cliUpdated, "cli_hooks_removed": cliRemoved,
		"vscode_hooks_config_path": paths.vscodeHooksConfigPath, "vscode_hooks_updated": vsUpdated, "vscode_hooks_removed": vsRemoved,
		"cli_supported": true, "vscode_supported": true,
	}, nil
}

func removeNestedHookCommands(path, rootKey string, events []string, field string, predicate func(string) bool) (bool, bool, error) {
	if !exists(path) {
		return false, false, nil
	}
	config, err := readJSON(path)
	if err != nil {
		return false, false, err
	}
	hooks := asObject(config[rootKey])
	updated := false
	for _, event := range events {
		entries, changed := removeCommands(asSlice(hooks[event]), predicate, field)
		updated = updated || changed
		if changed && len(entries) == 0 {
			delete(hooks, event)
		} else if changed {
			hooks[event] = entries
		}
	}
	if !updated {
		return false, false, nil
	}
	if len(hooks) == 0 {
		delete(config, rootKey)
	} else {
		config[rootKey] = hooks
	}
	if onlyVersionMetadata(config) {
		removed, err := remove(path)
		return true, removed, err
	}
	return true, false, writeJSON(path, config)
}

func onlyVersionMetadata(config object) bool {
	if len(config) == 0 {
		return true
	}
	return len(config) == 1 && config["version"] != nil
}

func scopeRoot(m *Manager, scope Scope) string {
	if scope == ScopeGlobal {
		return m.Home
	}
	return m.CWD
}
