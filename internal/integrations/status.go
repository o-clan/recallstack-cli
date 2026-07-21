package integrations

import "strings"

func (m *Manager) Status(scope Scope) (map[string]any, error) {
	if scope == "" {
		scope = ScopeLocal
	}
	if err := m.normalize(scope); err != nil {
		return nil, err
	}
	codex := m.codexPaths(scope)
	claude := m.claudePaths(scope)
	cursor := m.cursorPaths(scope)
	copilot := m.copilotPaths(scope)
	antigravity := m.antigravityPaths(scope)
	codexToml, err := readText(codex.configPath)
	if err != nil {
		return nil, err
	}
	codexStart, err := configEventHasManagedCommand(codex.hooksConfigPath, []string{"hooks", "SessionStart"}, "command", TargetCodex)
	if err != nil {
		return nil, err
	}
	codexStop, err := configEventHasManagedCommand(codex.hooksConfigPath, []string{"hooks", "Stop"}, "command", TargetCodex)
	if err != nil {
		return nil, err
	}
	codexHookInstalled := codexStart && codexStop
	claudeHookInstalled, err := configEventHasManagedCommand(claude.settingsPath, []string{"hooks", "Stop"}, "command", TargetClaude)
	if err != nil {
		return nil, err
	}
	cursorHookInstalled := true
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop"} {
		present, err := configEventHasManagedCommand(cursor.hooksConfigPath, []string{"hooks", event}, "command", TargetCursor)
		if err != nil {
			return nil, err
		}
		cursorHookInstalled = cursorHookInstalled && present
	}
	antigravityHookInstalled, err := configEventHasManagedCommand(antigravity.hooksConfigPath, []string{"recallstack", "Stop"}, "command", TargetAntigravity)
	if err != nil {
		return nil, err
	}
	if antiConfig, err := readJSON(antigravity.hooksConfigPath); err != nil {
		return nil, err
	} else if plugin := asObject(antiConfig["recallstack"]); plugin["enabled"] != true {
		antigravityHookInstalled = false
	}
	copilotHookInstalled := true
	for _, event := range []string{"sessionStart", "userPromptSubmitted", "sessionEnd"} {
		present, err := configEventHasManagedCommand(copilot.cliHooksConfigPath, []string{"hooks", event}, "bash", TargetCopilot)
		if err != nil {
			return nil, err
		}
		copilotHookInstalled = copilotHookInstalled && present
	}
	vsConfigPath := copilot.vscodeHooksConfigPath
	if scope == ScopeGlobal {
		vsConfigPath = copilot.cliHooksConfigPath
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		present, err := configEventHasManagedCommand(vsConfigPath, []string{"hooks", event}, "command", TargetCopilot)
		if err != nil {
			return nil, err
		}
		copilotHookInstalled = copilotHookInstalled && present
	}
	models := object{}
	for _, target := range []Target{TargetCodex, TargetClaude, TargetCopilot} {
		model, err := m.WorkerModel(target, scope)
		if err != nil {
			return nil, err
		}
		models[string(target)] = model
	}
	localSettings, err := m.readAgentSettings(ScopeLocal)
	if err != nil {
		return nil, err
	}
	globalSettings, err := m.readAgentSettings(ScopeGlobal)
	if err != nil {
		return nil, err
	}
	codexModel := models["codex"].(WorkerModelStatus)
	claudeModel := models["claude"].(WorkerModelStatus)
	copilotModel := models["copilot"].(WorkerModelStatus)
	copilotInstructionsInstalled := false
	if scope == ScopeLocal {
		current, err := readText(copilot.instructionsPath)
		if err != nil {
			return nil, err
		}
		copilotInstructionsInstalled = (strings.Contains(current, managedInstructionsStart) && strings.Contains(current, managedInstructionsEnd)) ||
			strings.TrimSpace(current) == strings.TrimSpace(string(copilotInstructions))
	}
	result := map[string]any{
		"scope":                      scope,
		"agent_settings_path":        m.agentSettingsPath(scope),
		"local_agent_settings_path":  m.agentSettingsPath(ScopeLocal),
		"global_agent_settings_path": m.agentSettingsPath(ScopeGlobal),
		"codex": object{
			"skill_dir": codex.skillDir, "skill_path": codex.skillPath, "skill_installed": exists(codex.skillPath),
			"skill_spec": "agentskills.io", "hook_command": m.hookCommand(TargetCodex, "stop"), "hook_installed": codexHookInstalled,
			"legacy_hook_path": codex.legacyHookPath, "legacy_hook_present": exists(codex.legacyHookPath),
			"hooks_config_path": codex.hooksConfigPath, "hooks_config_present": exists(codex.hooksConfigPath),
			"config_path": codex.configPath, "config_present": exists(codex.configPath),
			"repo_hooks_feature_enabled": tomlKeyEnabled(codexToml, "features", "codex_hooks", "true"),
			"worker_model":               codexModel.WorkerModel, "worker_model_source": codexModel.WorkerModelSource,
			"worker_model_change_command": codexModel.ChangeCommand, "worker_model_clear_command": codexModel.ClearCommand,
		},
		"claude": object{
			"skill_path": claude.skillPath, "skill_installed": exists(claude.skillPath),
			"settings_path": claude.settingsPath, "settings_present": exists(claude.settingsPath),
			"hook_command": m.hookCommand(TargetClaude, "stop"), "hook_installed": claudeHookInstalled,
			"legacy_hook_path": claude.legacyHookPath, "legacy_hook_present": exists(claude.legacyHookPath),
			"worker_model": claudeModel.WorkerModel, "worker_model_source": claudeModel.WorkerModelSource,
			"worker_model_change_command": claudeModel.ChangeCommand, "worker_model_clear_command": claudeModel.ClearCommand,
		},
		"cursor": object{
			"rule_path": cursor.rulePath, "rule_installed": exists(cursor.rulePath),
			"hooks_config_path": cursor.hooksConfigPath, "hooks_config_present": exists(cursor.hooksConfigPath),
			"hook_command": m.hookCommand(TargetCursor, "stop"), "hook_installed": cursorHookInstalled,
			"legacy_hook_path": cursor.legacyHookPath, "legacy_hook_present": exists(cursor.legacyHookPath),
			"ide_only": true, "cli_supported": false,
		},
		"worker_models": object{"local": localSettings, "global": globalSettings},
	}
	if scope == ScopeGlobal {
		result["copilot"] = object{
			"plugin_source_root": copilot.pluginSourceRoot, "plugin_source_present": exists(copilot.pluginSourceRoot),
			"plugin_manifest_path": copilot.pluginManifestPath, "plugin_manifest_present": exists(copilot.pluginManifestPath),
			"skill_path": copilot.skillPath, "skill_present": exists(copilot.skillPath),
			"hooks_config_path": copilot.cliHooksConfigPath, "hooks_config_present": exists(copilot.cliHooksConfigPath),
			"installed_plugin_path": copilot.installedPluginPath, "installed_plugin_present": exists(copilot.installedPluginPath),
			"config_dir": copilot.configDir, "hook_installed": copilotHookInstalled,
			"legacy_hook_path": copilot.legacyHookPath, "legacy_hook_present": exists(copilot.legacyHookPath),
			"cli_supported": true, "vscode_supported": true,
			"worker_model": copilotModel.WorkerModel, "worker_model_source": copilotModel.WorkerModelSource,
			"worker_model_change_command": copilotModel.ChangeCommand, "worker_model_clear_command": copilotModel.ClearCommand,
		}
		result["antigravity"] = object{
			"plugin_root": antigravity.root, "plugin_root_present": exists(antigravity.root),
			"plugin_manifest_path": antigravity.pluginManifestPath, "plugin_manifest_present": exists(antigravity.pluginManifestPath),
			"skill_path": antigravity.skillPath, "skill_installed": exists(antigravity.skillPath),
			"hooks_config_path": antigravity.hooksConfigPath, "hooks_config_present": exists(antigravity.hooksConfigPath),
			"hook_command": m.hookCommand(TargetAntigravity, "stop"), "hook_installed": antigravityHookInstalled,
			"legacy_hook_path": antigravity.legacyHookPath, "legacy_hook_present": exists(antigravity.legacyHookPath),
			"cli_plugin": true, "cli_supported": true, "ide_supported": true,
		}
	} else {
		result["copilot"] = object{
			"instructions_path": copilot.instructionsPath, "instructions_installed": copilotInstructionsInstalled,
			"cli_hooks_config_path": copilot.cliHooksConfigPath, "cli_hooks_config_present": exists(copilot.cliHooksConfigPath),
			"vscode_hooks_config_path": copilot.vscodeHooksConfigPath, "vscode_hooks_config_present": exists(copilot.vscodeHooksConfigPath),
			"hook_installed": copilotHookInstalled, "legacy_hook_path": copilot.legacyHookPath,
			"legacy_hook_present": exists(copilot.legacyHookPath), "cli_supported": true, "vscode_supported": true,
			"worker_model": copilotModel.WorkerModel, "worker_model_source": copilotModel.WorkerModelSource,
			"worker_model_change_command": copilotModel.ChangeCommand, "worker_model_clear_command": copilotModel.ClearCommand,
		}
		result["antigravity"] = object{
			"root": antigravity.root, "root_present": exists(antigravity.root),
			"skill_dir": antigravity.skillDir, "skill_path": antigravity.skillPath, "skill_installed": exists(antigravity.skillPath),
			"hooks_config_path": antigravity.hooksConfigPath, "hooks_config_present": exists(antigravity.hooksConfigPath),
			"hook_command": m.hookCommand(TargetAntigravity, "stop"), "hook_installed": antigravityHookInstalled,
			"legacy_hook_path": antigravity.legacyHookPath, "legacy_hook_present": exists(antigravity.legacyHookPath),
			"cli_plugin": false, "cli_supported": true, "ide_supported": true,
		}
	}
	return result, nil
}

func configEventHasManagedCommand(path string, keys []string, field string, target Target) (bool, error) {
	if !exists(path) {
		return false, nil
	}
	config, err := readJSON(path)
	if err != nil {
		return false, err
	}
	var value any = config
	for _, key := range keys {
		next := asObject(value)
		value = next[key]
	}
	return entriesContainMatchingCommand(asSlice(value), field, commandBelongsTo(target)), nil
}

func entriesContainMatchingCommand(entries []any, field string, matches func(string) bool) bool {
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if matches(strings.TrimSpace(stringField(item, field))) {
			return true
		}
		if entriesContainMatchingCommand(asSlice(item["hooks"]), field, matches) {
			return true
		}
	}
	return false
}
