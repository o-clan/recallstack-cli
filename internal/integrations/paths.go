package integrations

import "path/filepath"

type codexPaths struct {
	root, skillDir, skillPath, legacyHookPath, hooksConfigPath, configPath string
}

func (m *Manager) codexPaths(scope Scope) codexPaths {
	root := filepath.Join(m.CWD, ".codex")
	if scope == ScopeGlobal {
		root = filepath.Join(m.Home, ".codex")
	}
	skillDir := filepath.Join(root, "skills", "recallstack-memory")
	return codexPaths{
		root: root, skillDir: skillDir, skillPath: filepath.Join(skillDir, "SKILL.md"),
		legacyHookPath:  filepath.Join(root, "hooks", "recallstack.mjs"),
		hooksConfigPath: filepath.Join(root, "hooks.json"), configPath: filepath.Join(root, "config.toml"),
	}
}

type claudePaths struct {
	root, legacyHookPath, legacyShellPath, skillPath, settingsPath string
}

func (m *Manager) claudePaths(scope Scope) claudePaths {
	root := filepath.Join(m.CWD, ".claude")
	if scope == ScopeGlobal {
		root = filepath.Join(m.Home, ".claude")
	}
	settingsName := "settings.local.json"
	if scope == ScopeGlobal {
		settingsName = "settings.json"
	}
	return claudePaths{
		root: root, legacyHookPath: filepath.Join(root, "hooks", "recallstack.mjs"),
		legacyShellPath: filepath.Join(root, "hooks", "recallstack.sh"),
		skillPath:       filepath.Join(root, "skills", "recallstack-memory.md"),
		settingsPath:    filepath.Join(root, settingsName),
	}
}

type cursorPaths struct {
	root, rulePath, legacyHookPath, hooksConfigPath string
}

func (m *Manager) cursorPaths(scope Scope) cursorPaths {
	root := filepath.Join(m.CWD, ".cursor")
	if scope == ScopeGlobal {
		root = filepath.Join(m.Home, ".cursor")
	}
	return cursorPaths{
		root: root, rulePath: filepath.Join(root, "rules", "recallstack-memory.mdc"),
		legacyHookPath:  filepath.Join(root, "hooks", "recallstack.mjs"),
		hooksConfigPath: filepath.Join(root, "hooks.json"),
	}
}

type antigravityPaths struct {
	root, skillDir, skillPath, legacyHookPath, hooksConfigPath, pluginManifestPath string
}

func (m *Manager) antigravityPaths(scope Scope) antigravityPaths {
	if scope == ScopeGlobal {
		root := filepath.Join(m.Home, ".gemini", "antigravity-cli", "plugins", "recallstack")
		skillDir := filepath.Join(root, "skills", "recallstack-memory")
		return antigravityPaths{
			root: root, skillDir: skillDir, skillPath: filepath.Join(skillDir, "SKILL.md"),
			legacyHookPath:  filepath.Join(root, "hooks", "recallstack.mjs"),
			hooksConfigPath: filepath.Join(root, "hooks.json"), pluginManifestPath: filepath.Join(root, "plugin.json"),
		}
	}
	root := filepath.Join(m.CWD, ".agents")
	skillDir := filepath.Join(root, "skills", "recallstack-memory")
	return antigravityPaths{
		root: root, skillDir: skillDir, skillPath: filepath.Join(skillDir, "SKILL.md"),
		legacyHookPath:  filepath.Join(root, "hooks", "recallstack.mjs"),
		hooksConfigPath: filepath.Join(root, "hooks.json"),
	}
}

type copilotPaths struct {
	instructionsPath, pluginSourceRoot, pluginManifestPath, skillDir, skillPath string
	legacySkillPath, legacyHookPath, cliHooksConfigPath, vscodeHooksConfigPath  string
	installedPluginPath, configDir                                              string
}

func (m *Manager) copilotPaths(scope Scope) copilotPaths {
	if scope == ScopeGlobal {
		root := filepath.Join(m.Home, ".recallstack", "copilot-plugin")
		skillDir := filepath.Join(root, "skills", "recallstack-memory")
		configDir := filepath.Join(m.Home, ".copilot")
		return copilotPaths{
			pluginSourceRoot: root, pluginManifestPath: filepath.Join(root, "plugin.json"),
			skillDir: skillDir, skillPath: filepath.Join(skillDir, "SKILL.md"),
			legacySkillPath:     filepath.Join(root, "skills", "recallstack-memory.md"),
			legacyHookPath:      filepath.Join(root, "hooks", "recallstack.mjs"),
			cliHooksConfigPath:  filepath.Join(root, "hooks.json"),
			installedPluginPath: filepath.Join(configDir, "installed-plugins", "_direct", "copilot-plugin"),
			configDir:           configDir,
		}
	}
	hookDir := filepath.Join(m.CWD, ".github", "hooks")
	return copilotPaths{
		instructionsPath:      filepath.Join(m.CWD, ".github", "copilot-instructions.md"),
		legacyHookPath:        filepath.Join(hookDir, "recallstack.mjs"),
		cliHooksConfigPath:    filepath.Join(hookDir, "recallstack-copilot-cli.json"),
		vscodeHooksConfigPath: filepath.Join(hookDir, "recallstack-vscode.json"),
	}
}
