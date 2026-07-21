package integrations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type WorkerModelStatus struct {
	Target            Target `json:"target"`
	WorkerModel       any    `json:"worker_model"`
	WorkerModelSource any    `json:"worker_model_source"`
	ChangeCommand     string `json:"change_command"`
	ClearCommand      string `json:"clear_command"`
}

func (m *Manager) agentSettingsPath(scope Scope) string {
	if scope == ScopeGlobal {
		return filepath.Join(m.Home, ".recallstack", "agent-settings.json")
	}
	return filepath.Join(m.CWD, ".recallstack", "agent-settings.json")
}

func (m *Manager) AgentSettingsPath(scope Scope) string { return m.agentSettingsPath(scope) }

func (m *Manager) readAgentSettings(scope Scope) (object, error) {
	return readJSON(m.agentSettingsPath(scope))
}

func (m *Manager) SetWorkerModel(target Target, model string, scope Scope) (object, error) {
	if scope == "" {
		scope = ScopeLocal
	}
	if err := m.normalize(scope); err != nil {
		return nil, err
	}
	if !target.Valid() {
		return nil, fmt.Errorf("invalid target %q", target)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("worker model cannot be empty")
	}
	selected := modelTargets(target)
	if len(selected) == 0 {
		return object{"path": nil, "targets": []Target{}}, nil
	}
	settings, err := m.readAgentSettings(scope)
	if err != nil {
		return nil, err
	}
	for _, item := range selected {
		settings[string(item)] = object{"worker_model": model}
	}
	path := m.agentSettingsPath(scope)
	if err := writeJSON(path, settings); err != nil {
		return nil, err
	}
	return object{"path": path, "targets": selected}, nil
}

func (m *Manager) ClearWorkerModel(target Target, scope Scope) (object, error) {
	if scope == "" {
		scope = ScopeLocal
	}
	if err := m.normalize(scope); err != nil {
		return nil, err
	}
	if !target.Valid() {
		return nil, fmt.Errorf("invalid target %q", target)
	}
	selected := modelTargets(target)
	if len(selected) == 0 {
		return nil, nil
	}
	settings, err := m.readAgentSettings(scope)
	if err != nil {
		return nil, err
	}
	changed := false
	for _, item := range selected {
		if _, ok := settings[string(item)]; ok {
			delete(settings, string(item))
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	path := m.agentSettingsPath(scope)
	if len(settings) == 0 {
		if _, err := remove(path); err != nil {
			return nil, err
		}
	} else if err := writeJSON(path, settings); err != nil {
		return nil, err
	}
	return object{"path": path, "removed_targets": selected}, nil
}

func (m *Manager) WorkerModel(target Target, scope Scope) (WorkerModelStatus, error) {
	if scope == "" {
		scope = ScopeLocal
	}
	if err := m.normalize(scope); err != nil {
		return WorkerModelStatus{}, err
	}
	if len(modelTargets(target)) != 1 {
		return WorkerModelStatus{}, fmt.Errorf("worker models are supported for codex, claude, and copilot")
	}
	value, source, err := m.resolveWorkerModel(target, scope)
	if err != nil {
		return WorkerModelStatus{}, err
	}
	globalFlag := ""
	if scope == ScopeGlobal {
		globalFlag = " --global"
	}
	var jsonValue any
	var jsonSource any
	if value != "" {
		jsonValue = value
		jsonSource = string(source)
	}
	return WorkerModelStatus{
		Target:            target,
		WorkerModel:       jsonValue,
		WorkerModelSource: jsonSource,
		ChangeCommand:     fmt.Sprintf("recallstack agent model %s <model>%s", target, globalFlag),
		ClearCommand:      fmt.Sprintf("recallstack agent model %s --clear%s", target, globalFlag),
	}, nil
}

func (m *Manager) resolveWorkerModel(target Target, scope Scope) (string, Scope, error) {
	if scope == ScopeGlobal {
		settings, err := m.readAgentSettings(ScopeGlobal)
		if err != nil {
			return "", "", err
		}
		return modelFromSettings(settings, target), ScopeGlobal, nil
	}
	local, err := m.readAgentSettings(ScopeLocal)
	if err != nil {
		return "", "", err
	}
	if value := modelFromSettings(local, target); value != "" {
		return value, ScopeLocal, nil
	}
	global, err := m.readAgentSettings(ScopeGlobal)
	if err != nil {
		return "", "", err
	}
	if value := modelFromSettings(global, target); value != "" {
		return value, ScopeGlobal, nil
	}
	return "", "", nil
}

func modelFromSettings(settings object, target Target) string {
	value := strings.TrimSpace(stringField(settings[string(target)], "worker_model"))
	return value
}

func (m *Manager) ListWorkerModels(scope Scope) (object, error) {
	if scope == "" {
		scope = ScopeLocal
	}
	models := object{}
	for _, target := range []Target{TargetCodex, TargetClaude, TargetCopilot} {
		status, err := m.WorkerModel(target, scope)
		if err != nil {
			return nil, err
		}
		models[string(target)] = status
	}
	return object{"scope": scope, "models": models}, nil
}

func removeEmptySettingsDir(path string) {
	pruneEmpty([]string{filepath.Dir(path)}, filepath.Dir(filepath.Dir(path)))
}

func settingsFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
