package integrations

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

func hookCommand(agent Target, event string) string {
	return buildHookCommand("recallstack", agent, event)
}

func (m *Manager) hookCommand(agent Target, event string) string {
	return buildHookCommand(m.Executable, agent, event)
}

func buildHookCommand(executable string, agent Target, event string) string {
	return fmt.Sprintf("%s hook run %s %s", quoteExecutable(executable), agent, event)
}

func quoteExecutable(executable string) string {
	executable = strings.TrimSpace(executable)
	if executable == "recallstack" {
		return executable
	}
	if runtime.GOOS == "windows" {
		return `"` + executable + `"`
	}
	return "'" + strings.ReplaceAll(executable, "'", `'"'"'`) + "'"
}

func commandBelongsTo(agent Target) func(string) bool {
	marker := " hook run " + string(agent) + " "
	return func(command string) bool {
		command = strings.TrimSpace(command)
		if isLegacyRecallstackCommand(command) {
			return true
		}
		index := strings.LastIndex(command, marker)
		if index <= 0 || len(strings.Fields(command[index+len(marker):])) != 1 {
			return false
		}
		executable := strings.TrimSpace(command[:index])
		if len(executable) >= 2 && ((executable[0] == '\'' && executable[len(executable)-1] == '\'') || (executable[0] == '"' && executable[len(executable)-1] == '"')) {
			executable = executable[1 : len(executable)-1]
		}
		executable = strings.ReplaceAll(executable, `'"'"'`, "'")
		base := strings.ToLower(filepath.Base(executable))
		return base == "recallstack" || base == "recallstack.exe"
	}
}

func (m *Manager) Install(target Target, options InstallOptions) (map[string]any, error) {
	if options.Scope == "" {
		options.Scope = ScopeLocal
	}
	if err := m.normalize(options.Scope); err != nil {
		return nil, err
	}
	if !target.Valid() {
		return nil, fmt.Errorf("invalid target %q", target)
	}
	if strings.TrimSpace(options.WorkerModel) != "" {
		if _, err := m.SetWorkerModel(target, options.WorkerModel, options.Scope); err != nil {
			return nil, err
		}
	}
	out := map[string]any{}
	for _, item := range targets(target) {
		var result object
		var err error
		switch item {
		case TargetCodex:
			result, err = m.installCodex(options)
		case TargetClaude:
			result, err = m.installClaude(options)
		case TargetCursor:
			result, err = m.installCursor(options)
		case TargetCopilot:
			result, err = m.installCopilot(options)
		case TargetAntigravity:
			result, err = m.installAntigravity(options)
		}
		if err != nil {
			return nil, err
		}
		out[string(item)] = result
	}
	return out, nil
}

func (m *Manager) installCodex(options InstallOptions) (object, error) {
	paths := m.codexPaths(options.Scope)
	config, err := readJSON(paths.hooksConfigPath)
	if err != nil {
		return nil, err
	}
	toml, err := readText(paths.configPath)
	if err != nil {
		return nil, err
	}
	if err := writeFile(paths.skillPath, codexSkill, 0o644); err != nil {
		return nil, err
	}
	hooks := asObject(config["hooks"])
	ensureCodexHook(hooks, "SessionStart", m.hookCommand(TargetCodex, "session_start"), "startup|resume|clear")
	ensureCodexHook(hooks, "Stop", m.hookCommand(TargetCodex, "stop"), "")
	config["hooks"] = hooks
	if err := writeJSON(paths.hooksConfigPath, config); err != nil {
		return nil, err
	}
	if err := writeFile(paths.configPath, []byte(upsertTomlKey(toml, "features", "codex_hooks", "true")), 0o644); err != nil {
		return nil, err
	}
	legacyRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	return object{
		"scope": options.Scope, "skill_dir": paths.skillDir, "skill_path": paths.skillPath,
		"skill_spec": "agentskills.io", "hook_command": m.hookCommand(TargetCodex, "stop"),
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"hooks_config_path": paths.hooksConfigPath, "config_path": paths.configPath,
		"worker_model": nullableString(options.WorkerModel),
	}, nil
}

func ensureCodexHook(hooks object, event, command, matcher string) {
	entries, _ := removeCommands(asSlice(hooks[event]), commandBelongsTo(TargetCodex), "command")
	hook := object{"type": "command", "command": command, "timeout": float64(180)}
	entry := object{"hooks": []any{hook}}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	hooks[event] = append(entries, entry)
}

func (m *Manager) installClaude(options InstallOptions) (object, error) {
	paths := m.claudePaths(options.Scope)
	settings, err := readJSON(paths.settingsPath)
	if err != nil {
		return nil, err
	}
	if err := writeFile(paths.skillPath, claudeSkill, 0o644); err != nil {
		return nil, err
	}
	hooks := asObject(settings["hooks"])
	for _, event := range []string{"UserPromptSubmit", "Stop"} {
		entries, changed := removeCommands(asSlice(hooks[event]), commandBelongsTo(TargetClaude), "command")
		if changed && len(entries) == 0 {
			delete(hooks, event)
		} else if changed {
			hooks[event] = entries
		}
	}
	command := m.hookCommand(TargetClaude, "stop")
	stop := asSlice(hooks["Stop"])
	stop = append(stop, object{"hooks": []any{object{
		"type": "command", "command": command, "async": true, "timeout": float64(180),
	}}})
	hooks["Stop"] = stop
	settings["hooks"] = hooks
	if err := writeJSON(paths.settingsPath, settings); err != nil {
		return nil, err
	}
	legacyMJSRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	legacyShellRemoved, err := remove(paths.legacyShellPath)
	if err != nil {
		return nil, err
	}
	return object{
		"scope": options.Scope, "skill_path": paths.skillPath, "settings_path": paths.settingsPath,
		"hook_command": command, "legacy_hook_path": paths.legacyHookPath,
		"legacy_hook_removed": legacyMJSRemoved || legacyShellRemoved,
		"worker_model":        nullableString(options.WorkerModel),
	}, nil
}

func (m *Manager) installCursor(options InstallOptions) (object, error) {
	paths := m.cursorPaths(options.Scope)
	config, err := readJSON(paths.hooksConfigPath)
	if err != nil {
		return nil, err
	}
	if err := writeFile(paths.rulePath, cursorRule, 0o644); err != nil {
		return nil, err
	}
	config["version"] = float64(1)
	hooks := asObject(config["hooks"])
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop"} {
		entries, _ := removeCommands(asSlice(hooks[event]), commandBelongsTo(TargetCursor), "command")
		hooks[event] = append(entries, object{"command": m.hookCommand(TargetCursor, event)})
	}
	config["hooks"] = hooks
	if err := writeJSON(paths.hooksConfigPath, config); err != nil {
		return nil, err
	}
	legacyRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	return object{
		"scope": options.Scope, "rule_path": paths.rulePath,
		"hooks_config_path": paths.hooksConfigPath, "hook_command": m.hookCommand(TargetCursor, "stop"),
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"ide_only": true,
	}, nil
}

func (m *Manager) installAntigravity(options InstallOptions) (object, error) {
	paths := m.antigravityPaths(options.Scope)
	config, err := readJSON(paths.hooksConfigPath)
	if err != nil {
		return nil, err
	}
	if err := writeFile(paths.skillPath, antigravitySkill, 0o644); err != nil {
		return nil, err
	}
	plugin := asObject(config["recallstack"])
	entries, _ := removeCommands(asSlice(plugin["Stop"]), commandBelongsTo(TargetAntigravity), "command")
	command := m.hookCommand(TargetAntigravity, "stop")
	plugin["enabled"] = true
	plugin["Stop"] = append(entries, object{"type": "command", "command": command, "timeout": float64(10)})
	config["recallstack"] = plugin
	if err := writeJSON(paths.hooksConfigPath, config); err != nil {
		return nil, err
	}
	if options.Scope == ScopeGlobal {
		manifest := object{
			"name": "recallstack", "version": m.Version,
			"description": "Recallstack memory retrieval guidance and Antigravity Stop hook automation.",
			"skills":      []any{"skills/"}, "hooks": "hooks.json",
		}
		if err := writeJSON(paths.pluginManifestPath, manifest); err != nil {
			return nil, err
		}
	}
	legacyRemoved, err := remove(paths.legacyHookPath)
	if err != nil {
		return nil, err
	}
	return object{
		"scope": options.Scope, "root": paths.root, "skill_dir": paths.skillDir,
		"skill_path": paths.skillPath, "hooks_config_path": paths.hooksConfigPath,
		"plugin_manifest_path": nullablePath(paths.pluginManifestPath), "hook_command": command,
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"cli_plugin": options.Scope == ScopeGlobal,
	}, nil
}

func (m *Manager) installCopilot(options InstallOptions) (object, error) {
	paths := m.copilotPaths(options.Scope)
	if _, err := readJSON(paths.cliHooksConfigPath); err != nil {
		return nil, err
	}
	if options.Scope == ScopeLocal {
		if _, err := readJSON(paths.vscodeHooksConfigPath); err != nil {
			return nil, err
		}
	}
	if options.Scope == ScopeGlobal {
		if _, err := remove(paths.legacySkillPath); err != nil {
			return nil, err
		}
		if err := writeFile(paths.skillPath, copilotPluginSkill, 0o644); err != nil {
			return nil, err
		}
		if err := writeJSON(paths.pluginManifestPath, object{
			"name": "recallstack", "version": m.Version,
			"description": "Recallstack memory retrieval guidance and hook automation.",
			"skills":      []any{"skills/"}, "hooks": "hooks.json",
		}); err != nil {
			return nil, err
		}
	} else {
		current, err := readText(paths.instructionsPath)
		if err != nil {
			return nil, err
		}
		if err := writeFile(paths.instructionsPath, []byte(mergeManagedInstructions(current, copilotInstructions)), 0o644); err != nil {
			return nil, err
		}
	}
	if options.Scope == ScopeGlobal {
		if err := m.installCopilotHookConfig(paths.cliHooksConfigPath, true); err != nil {
			return nil, err
		}
	} else {
		if err := m.installCopilotHookConfig(paths.cliHooksConfigPath, false); err != nil {
			return nil, err
		}
		if err := m.installCopilotVSCodeConfig(paths.vscodeHooksConfigPath); err != nil {
			return nil, err
		}
	}
	legacyRemoved := false
	reinstalled := false
	var err error
	if options.Scope == ScopeGlobal {
		uninstall, _ := m.Run("copilot", "plugin", "uninstall", "recallstack", "--config-dir", paths.configDir)
		reinstalled = uninstall.Status == 0
		installed, installErr := m.Run("copilot", "plugin", "install", paths.pluginSourceRoot, "--config-dir", paths.configDir)
		if installErr != nil || installed.Status != 0 {
			detail := strings.TrimSpace(installed.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(installed.Stdout)
			}
			if detail == "" && installErr != nil {
				detail = installErr.Error()
			}
			return nil, fmt.Errorf("failed to install global Copilot plugin: %s", detail)
		}
		legacyRemoved, err = remove(paths.legacyHookPath)
		if err != nil {
			return nil, err
		}
	} else {
		legacyRemoved, err = remove(paths.legacyHookPath)
		if err != nil {
			return nil, err
		}
	}
	result := object{
		"scope": options.Scope, "worker_model": nullableString(options.WorkerModel),
		"legacy_hook_path": paths.legacyHookPath, "legacy_hook_removed": legacyRemoved,
		"cli_hooks_config_path": paths.cliHooksConfigPath, "cli_supported": true, "vscode_supported": true,
	}
	if options.Scope == ScopeGlobal {
		result["plugin_source_root"] = paths.pluginSourceRoot
		result["plugin_manifest_path"] = paths.pluginManifestPath
		result["skill_path"] = paths.skillPath
		result["installed_plugin_path"] = paths.installedPluginPath
		result["config_dir"] = paths.configDir
		result["reinstall"] = reinstalled
	} else {
		result["instructions_path"] = paths.instructionsPath
		result["vscode_hooks_config_path"] = paths.vscodeHooksConfigPath
	}
	return result, nil
}

func (m *Manager) installCopilotHookConfig(path string, includeVSCode bool) error {
	config, err := readJSON(path)
	if err != nil {
		return err
	}
	config["version"] = float64(1)
	hooks := asObject(config["hooks"])
	for _, event := range []string{"sessionStart", "userPromptSubmitted", "sessionEnd"} {
		entries, _ := removeCommands(asSlice(hooks[event]), commandBelongsTo(TargetCopilot), "bash")
		hooks[event] = append(entries, object{"type": "command", "bash": m.hookCommand(TargetCopilot, event)})
	}
	if includeVSCode {
		for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
			entries, _ := removeCommands(asSlice(hooks[event]), commandBelongsTo(TargetCopilot), "command")
			hooks[event] = append(entries, object{"command": m.hookCommand(TargetCopilot, event)})
		}
	}
	config["hooks"] = hooks
	return writeJSON(path, config)
}

func (m *Manager) installCopilotVSCodeConfig(path string) error {
	config, err := readJSON(path)
	if err != nil {
		return err
	}
	config["version"] = float64(1)
	hooks := asObject(config["hooks"])
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		entries, _ := removeCommands(asSlice(hooks[event]), commandBelongsTo(TargetCopilot), "command")
		hooks[event] = append(entries, object{"command": m.hookCommand(TargetCopilot, event)})
	}
	config["hooks"] = hooks
	return writeJSON(path, config)
}

func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullablePath(value string) any {
	if value == "" {
		return nil
	}
	return filepath.Clean(value)
}
