package integrations

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "workspace")
	for _, path := range []string{home, cwd} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{
		CWD: cwd, Home: home, Version: "1.2.3",
		Run: func(string, ...string) (CommandResult, error) { return CommandResult{Status: 0}, nil },
	}
}

func mustWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := writeJSON(path, value); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := writeFile(path, []byte(value), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLocalInstallUpgradesLegacyHooksAndPreservesConfiguration(t *testing.T) {
	m := testManager(t)
	codex := m.codexPaths(ScopeLocal)
	claude := m.claudePaths(ScopeLocal)
	cursor := m.cursorPaths(ScopeLocal)
	copilot := m.copilotPaths(ScopeLocal)
	antigravity := m.antigravityPaths(ScopeLocal)

	for _, path := range []string{
		codex.legacyHookPath, claude.legacyHookPath, claude.legacyShellPath,
		cursor.legacyHookPath, copilot.legacyHookPath, antigravity.legacyHookPath,
	} {
		mustWrite(t, path, "#!/usr/bin/env node\n")
	}
	mustWriteJSON(t, codex.hooksConfigPath, object{
		"custom": "keep", "hooks": object{
			"SessionStart": []any{
				object{"hooks": []any{object{"type": "command", "command": "node ./.codex/hooks/recallstack.mjs session_start"}}},
			},
			"Other": []any{
				object{"hooks": []any{object{"type": "command", "command": "keep-me"}}},
			},
		},
	})
	mustWrite(t, codex.configPath, "theme = \"dark\"\n\n[features]\nother = true\n")
	mustWriteJSON(t, claude.settingsPath, object{
		"permissions": object{"allow": []any{"Read"}},
		"hooks": object{"Stop": []any{object{"hooks": []any{
			object{"type": "command", "command": "node '.claude/hooks/recallstack.mjs' stop"},
			object{"type": "command", "command": "keep-claude"},
		}}}},
	})
	mustWriteJSON(t, cursor.hooksConfigPath, object{
		"custom": true, "hooks": object{
			"stop":  []any{object{"command": "node ./.cursor/hooks/recallstack.mjs"}},
			"other": []any{object{"command": "keep-cursor"}},
		},
	})
	mustWriteJSON(t, antigravity.hooksConfigPath, object{
		"other-plugin": object{"enabled": true},
		"recallstack":  object{"Stop": []any{object{"type": "command", "command": "node ./.agents/hooks/recallstack.mjs stop"}}},
	})
	mustWriteJSON(t, copilot.cliHooksConfigPath, object{
		"extra": true, "hooks": object{"sessionStart": []any{
			object{"type": "command", "bash": "node ./.github/hooks/recallstack.mjs sessionStart"},
			object{"type": "command", "bash": "keep-copilot"},
		}},
	})
	mustWrite(t, copilot.instructionsPath, "# Project instructions\n\nKeep this guidance.\n")

	installed, err := m.Install(TargetAll, InstallOptions{Scope: ScopeLocal, WorkerModel: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 5 {
		t.Fatalf("installed %d integrations, want 5", len(installed))
	}
	for _, path := range []string{
		codex.legacyHookPath, claude.legacyHookPath, claude.legacyShellPath,
		cursor.legacyHookPath, copilot.legacyHookPath, antigravity.legacyHookPath,
	} {
		if exists(path) {
			t.Errorf("legacy runtime still exists: %s", path)
		}
	}
	for _, path := range []string{
		codex.hooksConfigPath, claude.settingsPath, cursor.hooksConfigPath,
		copilot.cliHooksConfigPath, copilot.vscodeHooksConfigPath, antigravity.hooksConfigPath,
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if strings.Contains(text, ".mjs") || strings.Contains(text, "node ") {
			t.Errorf("legacy Node command remains in %s:\n%s", path, text)
		}
	}

	assertFileEquals(t, codex.skillPath, codexSkill)
	assertFileEquals(t, claude.skillPath, claudeSkill)
	assertFileEquals(t, cursor.rulePath, cursorRule)
	assertJSONContains(t, copilot.instructionsPath, "# Project instructions", "Keep this guidance.", managedInstructionsStart, string(copilotInstructions))
	assertFileEquals(t, antigravity.skillPath, antigravitySkill)
	assertJSONContains(t, codex.hooksConfigPath, "keep-me", "recallstack hook run codex stop")
	assertJSONContains(t, claude.settingsPath, "keep-claude", "recallstack hook run claude stop")
	assertJSONContains(t, cursor.hooksConfigPath, "keep-cursor", "recallstack hook run cursor stop")
	assertJSONContains(t, copilot.cliHooksConfigPath, "keep-copilot", "recallstack hook run copilot sessionEnd")
	assertJSONContains(t, antigravity.hooksConfigPath, "other-plugin", "recallstack hook run antigravity stop")

	toml, err := readText(codex.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toml, "theme = \"dark\"") || !strings.Contains(toml, "other = true") || !tomlKeyEnabled(toml, "features", "codex_hooks", "true") {
		t.Fatalf("Codex TOML was not merged:\n%s", toml)
	}

	status, err := m.Status(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"codex", "claude", "cursor", "copilot", "antigravity"} {
		entry := status[target].(object)
		if entry["hook_installed"] != true {
			t.Errorf("%s hook status = %v, want true", target, entry["hook_installed"])
		}
	}

	settings, err := m.readAgentSettings(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{TargetCodex, TargetClaude, TargetCopilot} {
		if got := modelFromSettings(settings, target); got != "worker-v1" {
			t.Errorf("%s model = %q", target, got)
		}
	}
}

func TestUninstallRemovesOwnedEntriesAndKeepsUnrelatedConfiguration(t *testing.T) {
	m := testManager(t)
	copilot := m.copilotPaths(ScopeLocal)
	mustWrite(t, copilot.instructionsPath, "# Project instructions\n\nKeep this guidance.\n")
	if _, err := m.Install(TargetAll, InstallOptions{Scope: ScopeLocal, WorkerModel: "worker-v1"}); err != nil {
		t.Fatal(err)
	}
	codex := m.codexPaths(ScopeLocal)
	claude := m.claudePaths(ScopeLocal)
	cursor := m.cursorPaths(ScopeLocal)
	antigravity := m.antigravityPaths(ScopeLocal)

	addHookEntry(t, codex.hooksConfigPath, "hooks", "Stop", object{"hooks": []any{object{"command": "keep-codex"}}})
	addHookEntry(t, claude.settingsPath, "hooks", "Stop", object{"hooks": []any{object{"command": "keep-claude"}}})
	addHookEntry(t, cursor.hooksConfigPath, "hooks", "stop", object{"command": "keep-cursor"})
	config, err := readJSON(antigravity.hooksConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	config["other-plugin"] = object{"enabled": true}
	mustWriteJSON(t, antigravity.hooksConfigPath, config)

	removed, err := m.Uninstall(TargetAll, UninstallOptions{Scope: ScopeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 5 {
		t.Fatalf("removed %d integrations, want 5", len(removed))
	}
	for _, path := range []string{codex.skillPath, claude.skillPath, cursor.rulePath, antigravity.skillPath} {
		if exists(path) {
			t.Errorf("owned asset still exists: %s", path)
		}
	}
	assertJSONContains(t, codex.hooksConfigPath, "keep-codex")
	assertJSONContains(t, claude.settingsPath, "keep-claude")
	assertJSONContains(t, cursor.hooksConfigPath, "keep-cursor")
	assertJSONContains(t, antigravity.hooksConfigPath, "other-plugin")
	assertJSONContains(t, copilot.instructionsPath, "# Project instructions", "Keep this guidance.")
	instructions, err := os.ReadFile(copilot.instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(instructions), managedInstructionsStart) || strings.Contains(string(instructions), "# Recallstack Memory") {
		t.Fatalf("Recallstack managed instructions remain after uninstall:\n%s", instructions)
	}
	for _, path := range []string{codex.hooksConfigPath, claude.settingsPath, cursor.hooksConfigPath, antigravity.hooksConfigPath} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "recallstack hook run") {
			t.Errorf("native command remains after uninstall in %s", path)
		}
	}
	if exists(m.agentSettingsPath(ScopeLocal)) {
		t.Fatal("worker model settings still exist after uninstall all")
	}
}

func TestWorkerModelsResolveLocalOverGlobalAndClear(t *testing.T) {
	m := testManager(t)
	if _, err := m.SetWorkerModel(TargetCodex, "global-model", ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	status, err := m.WorkerModel(TargetCodex, ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if status.WorkerModel != "global-model" || status.WorkerModelSource != string(ScopeGlobal) {
		t.Fatalf("unexpected inherited model: %#v", status)
	}
	if _, err := m.SetWorkerModel(TargetCodex, "local-model", ScopeLocal); err != nil {
		t.Fatal(err)
	}
	status, err = m.WorkerModel(TargetCodex, ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if status.WorkerModel != "local-model" || status.WorkerModelSource != string(ScopeLocal) {
		t.Fatalf("unexpected local model: %#v", status)
	}
	if _, err := m.ClearWorkerModel(TargetCodex, ScopeLocal); err != nil {
		t.Fatal(err)
	}
	status, err = m.WorkerModel(TargetCodex, ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if status.WorkerModel != "global-model" || status.WorkerModelSource != string(ScopeGlobal) {
		t.Fatalf("global model did not reappear after clear: %#v", status)
	}
}

func TestGlobalInstallUsesHomeAndNativeCopilotPluginCommands(t *testing.T) {
	m := testManager(t)
	var calls [][]string
	m.Run = func(name string, args ...string) (CommandResult, error) {
		calls = append(calls, append([]string{name}, args...))
		return CommandResult{Status: 0}, nil
	}
	installed, err := m.Install(TargetAll, InstallOptions{Scope: ScopeGlobal})
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 5 {
		t.Fatalf("installed %d integrations, want 5", len(installed))
	}
	if len(calls) != 2 || calls[0][2] != "uninstall" || calls[1][2] != "install" {
		t.Fatalf("unexpected Copilot plugin calls: %#v", calls)
	}
	for _, path := range []string{
		m.codexPaths(ScopeGlobal).skillPath,
		m.claudePaths(ScopeGlobal).skillPath,
		m.cursorPaths(ScopeGlobal).rulePath,
		m.copilotPaths(ScopeGlobal).skillPath,
		m.antigravityPaths(ScopeGlobal).skillPath,
	} {
		if !pathWithin(path, m.Home) || !exists(path) {
			t.Errorf("global asset is missing or outside home: %s", path)
		}
	}
	assertJSONContains(t, m.copilotPaths(ScopeGlobal).cliHooksConfigPath,
		"recallstack hook run copilot sessionStart", "recallstack hook run copilot Stop")
	assertFileEquals(t, m.copilotPaths(ScopeGlobal).skillPath, copilotPluginSkill)
}

func TestInstalledHooksUseAbsoluteExecutableAndRemainManageable(t *testing.T) {
	m := testManager(t)
	m.Executable = filepath.Join(t.TempDir(), "Recall Stack's", "recallstack")
	if _, err := m.Install(TargetCursor, InstallOptions{Scope: ScopeLocal}); err != nil {
		t.Fatal(err)
	}

	expected := buildHookCommand(m.Executable, TargetCursor, "stop")
	config, err := readJSON(m.cursorPaths(ScopeLocal).hooksConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	stopEntries := asSlice(asObject(config["hooks"])["stop"])
	if len(stopEntries) != 1 || stringField(asObject(stopEntries[0]), "command") != expected {
		t.Fatalf("absolute hook command was not installed: %#v", stopEntries)
	}
	if !commandBelongsTo(TargetCursor)(expected) {
		t.Fatalf("absolute native command was not recognized: %s", expected)
	}

	m.Executable = filepath.Join(t.TempDir(), "moved", "recallstack")
	status, err := m.Status(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if status["cursor"].(object)["hook_installed"] != true {
		t.Fatal("status did not recognize an installed hook after the executable moved")
	}
	if _, err := m.Uninstall(TargetCursor, UninstallOptions{Scope: ScopeLocal}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(m.cursorPaths(ScopeLocal).hooksConfigPath)
	if err == nil && strings.Contains(string(body), expected) {
		t.Fatal("uninstall left the absolute native hook command behind")
	}
}

func TestMalformedExistingJSONFailsWithoutOverwriting(t *testing.T) {
	m := testManager(t)
	paths := m.cursorPaths(ScopeLocal)
	path := paths.hooksConfigPath
	mustWrite(t, path, "{invalid")
	mustWrite(t, paths.legacyHookPath, "#!/usr/bin/env node\n")
	_, err := m.Install(TargetCursor, InstallOptions{Scope: ScopeLocal})
	if err == nil {
		t.Fatal("expected malformed JSON error")
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "{invalid" {
		t.Fatalf("malformed config was overwritten: %q", body)
	}
	if !exists(paths.legacyHookPath) {
		t.Fatal("failed upgrade removed the working legacy hook")
	}
}

func TestStatusRequiresCommandsInTheExpectedEvents(t *testing.T) {
	m := testManager(t)
	paths := m.cursorPaths(ScopeLocal)
	mustWriteJSON(t, paths.hooksConfigPath, object{
		"metadata": []any{
			hookCommand(TargetCursor, "beforeSubmitPrompt"),
			hookCommand(TargetCursor, "afterAgentResponse"),
			hookCommand(TargetCursor, "stop"),
		},
		"hooks": object{"wrongEvent": []any{object{"command": hookCommand(TargetCursor, "stop")}}},
	})
	status, err := m.Status(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if status["cursor"].(object)["hook_installed"] != false {
		t.Fatal("misplaced commands produced a false installed status")
	}
}

func TestCopilotManagedInstructionsAreIdempotent(t *testing.T) {
	m := testManager(t)
	paths := m.copilotPaths(ScopeLocal)
	mustWrite(t, paths.instructionsPath, "# Project guidance\n")
	if _, err := m.Install(TargetCopilot, InstallOptions{Scope: ScopeLocal}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(TargetCopilot, InstallOptions{Scope: ScopeLocal}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(paths.instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), managedInstructionsStart) != 1 || strings.Count(string(body), "# Project guidance") != 1 {
		t.Fatalf("managed instructions were duplicated:\n%s", body)
	}
}

func TestParseHelpers(t *testing.T) {
	target, err := ParseTarget(" CoDeX ")
	if err != nil || target != TargetCodex {
		t.Fatalf("ParseTarget = %q, %v", target, err)
	}
	if _, err := ParseTarget("unknown"); err == nil {
		t.Fatal("expected unsupported target error")
	}
	if scope, err := ParseScope(""); err != nil || scope != ScopeLocal {
		t.Fatalf("ParseScope = %q, %v", scope, err)
	}
}

func assertFileEquals(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("file %s does not match embedded asset", path)
	}
}

func assertJSONContains(t *testing.T, path string, values ...string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	for _, value := range values {
		value = strings.ReplaceAll(value, "\r\n", "\n")
		if !strings.Contains(text, value) {
			t.Errorf("%s does not contain %q:\n%s", path, value, body)
		}
	}
}

func addHookEntry(t *testing.T, path, root, event string, entry object) {
	t.Helper()
	config, err := readJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	hooks := asObject(config[root])
	hooks[event] = append(asSlice(hooks[event]), entry)
	config[root] = hooks
	mustWriteJSON(t, path, config)
}

func TestRunnerErrorIsReportedForGlobalCopilotInstall(t *testing.T) {
	m := testManager(t)
	m.Run = func(_ string, args ...string) (CommandResult, error) {
		if len(args) > 1 && args[1] == "install" {
			return CommandResult{Status: 1, Stderr: "plugin failed"}, errors.New("exit status 1")
		}
		return CommandResult{Status: 0}, nil
	}
	_, err := m.Install(TargetCopilot, InstallOptions{Scope: ScopeGlobal})
	if err == nil || !strings.Contains(err.Error(), "plugin failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestJSONOutputTypesMarshal(t *testing.T) {
	m := testManager(t)
	status, err := m.Status(ScopeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(status); err != nil {
		t.Fatalf("status is not JSON-ready: %v", err)
	}
}
