package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Mode string

const (
	ModeGlobal  Mode = "GLOBAL"
	ModeProject Mode = "PROJECT"
)

type Workspace struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type Project struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Mode Mode   `json:"mode"`
}

type Resolved struct {
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceSlug string `json:"workspace_slug"`
	WorkspaceName string `json:"workspace_name"`
	ProjectID     string `json:"project_id"`
	ProjectSlug   string `json:"project_slug"`
	ProjectName   string `json:"project_name"`
	Mode          Mode   `json:"project_mode"`
}

func (r Resolved) Canonical() string { return r.WorkspaceSlug + "/" + r.ProjectSlug }

type Backend interface {
	ListWorkspaces(context.Context) ([]Workspace, error)
	ListProjects(context.Context, string) ([]Project, error)
}

type Resolver struct {
	Backend Backend
	Warn    io.Writer
	Now     func() time.Time
}

func ParseCanonical(value string) (workspaceSlug, projectSlug string, err error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", "", errors.New("Target cannot be empty. Use <workspaceSlug>/<projectSlug>.")
	}
	parts := strings.Split(normalized, "/")
	compact := parts[:0]
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			compact = append(compact, part)
		}
	}
	if len(compact) != 2 {
		return "", "", fmt.Errorf("Invalid target %q. Use <workspaceSlug>/<projectSlug>.", value)
	}
	return compact[0], compact[1], nil
}

func WorkspaceConfigPath(cwd string) string {
	return filepath.Join(cwd, ".recallstack", "workspace.json")
}

func FindWorkspaceConfig(startCWD string) (string, map[string]any, error) {
	cursor, err := filepath.Abs(startCWD)
	if err != nil {
		return "", nil, err
	}
	for {
		candidate := WorkspaceConfigPath(cursor)
		data, readErr := os.ReadFile(candidate)
		if readErr == nil {
			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				return "", nil, fmt.Errorf("Failed to parse JSON file at %s. Fix the file and retry.", candidate)
			}
			if cfg == nil {
				cfg = map[string]any{}
			}
			return candidate, cfg, nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return "", nil, readErr
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "", map[string]any{}, nil
		}
		cursor = parent
	}
}

func (r *Resolver) Save(cwd string, value Resolved) (string, error) {
	path := WorkspaceConfigPath(cwd)
	cfg, err := readObject(path)
	if err != nil {
		return "", err
	}
	cfg["workspace_id"] = value.WorkspaceID
	cfg["workspace_slug"] = value.WorkspaceSlug
	cfg["workspace_name"] = value.WorkspaceName
	cfg["project_id"] = value.ProjectID
	cfg["project_slug"] = value.ProjectSlug
	cfg["project_name"] = value.ProjectName
	cfg["project_mode"] = value.Mode
	delete(cfg, "project")
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	cfg["workspace_root"] = abs
	cfg["updated_at"] = r.now().UTC().Format(time.RFC3339Nano)
	return path, writeObject(path, cfg)
}

func (r *Resolver) Clear(cwd string) (string, error) {
	found, cfg, err := FindWorkspaceConfig(cwd)
	if err != nil {
		return "", err
	}
	path := found
	if path == "" {
		path = WorkspaceConfigPath(cwd)
	}
	for _, key := range []string{"workspace_id", "workspace_slug", "workspace_name", "project_id", "project_slug", "project_name", "project_mode", "project"} {
		delete(cfg, key)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	cfg["workspace_root"] = abs
	cfg["updated_at"] = r.now().UTC().Format(time.RFC3339Nano)
	return path, writeObject(path, cfg)
}

func (r *Resolver) ResolveInput(ctx context.Context, value, cwd string, preferConfigured *Workspace) (Resolved, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return Resolved{}, errors.New("Project value cannot be empty. Use <projectSlug> or <workspaceSlug/projectSlug>.")
	}
	if strings.Contains(normalized, "/") {
		ws, project, err := ParseCanonical(normalized)
		if err != nil {
			return Resolved{}, err
		}
		return r.resolveCanonical(ctx, ws, project)
	}
	var workspace *Workspace
	if preferConfigured != nil {
		copy := *preferConfigured
		workspace = &copy
	}
	if workspace == nil {
		if local, _ := r.LoadLocal(ctx, cwd); local != nil {
			workspace = &Workspace{ID: local.WorkspaceID, Slug: local.WorkspaceSlug, Name: local.WorkspaceName}
		}
	}
	if workspace != nil {
		resolved, found, err := r.projectInWorkspace(ctx, *workspace, normalized)
		if err != nil {
			return Resolved{}, err
		}
		if found {
			return resolved, nil
		}
		return Resolved{}, fmt.Errorf("Project %q was not found in workspace %q. Use `recallstack project list --workspace %s` or pass <workspaceSlug/projectSlug>.", normalized, workspace.Slug, workspace.Slug)
	}
	workspaces, err := r.Backend.ListWorkspaces(ctx)
	if err != nil {
		return Resolved{}, err
	}
	var matches []Resolved
	for _, ws := range workspaces {
		match, found, err := r.projectInWorkspace(ctx, ws, normalized)
		if err != nil {
			return Resolved{}, err
		}
		if found {
			matches = append(matches, match)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return Resolved{}, fmt.Errorf("Project %q was not found. Run `recallstack project use` or pass <workspaceSlug/projectSlug>.", normalized)
	}
	choices := make([]string, len(matches))
	for i := range matches {
		choices[i] = matches[i].Canonical()
	}
	return Resolved{}, fmt.Errorf("Project %q is ambiguous across workspaces: %s. Use <workspaceSlug/projectSlug>.", normalized, strings.Join(choices, ", "))
}

func (r *Resolver) ResolveForCommand(ctx context.Context, cwd, explicit string, requireWritable bool) (Resolved, error) {
	var value Resolved
	var err error
	if strings.TrimSpace(explicit) != "" {
		value, err = r.ResolveInput(ctx, explicit, cwd, nil)
	} else {
		loaded, loadErr := r.LoadLocal(ctx, cwd)
		err = loadErr
		if loaded != nil {
			value = *loaded
		} else if err == nil {
			err = errors.New("No default project configured for this directory. Run `recallstack project use` (or `recallstack workspace use`) or pass `--project <projectSlug|workspaceSlug/projectSlug>`.")
		}
	}
	if err != nil {
		return Resolved{}, err
	}
	if requireWritable && value.Mode == ModeGlobal {
		return Resolved{}, errors.New("GLOBAL_WRITE_FORBIDDEN: raw memory writes cannot target global. Choose a non-global project with `recallstack project use`.")
	}
	return value, nil
}

func (r *Resolver) LoadLocal(ctx context.Context, cwd string) (*Resolved, error) {
	_, cfg, err := FindWorkspaceConfig(cwd)
	if err != nil {
		return nil, err
	}
	wsID, wsSlug := stringValue(cfg["workspace_id"]), stringValue(cfg["workspace_slug"])
	projectID, projectSlug := stringValue(cfg["project_id"]), stringValue(cfg["project_slug"])
	fallback := storedFallback(cfg)
	if wsSlug != "" && projectSlug != "" {
		resolved, resolveErr := r.resolveCanonical(ctx, wsSlug, projectSlug)
		if resolveErr == nil {
			if wsID != resolved.WorkspaceID || projectID != resolved.ProjectID {
				_, _ = r.Save(cwd, resolved)
			}
			return &resolved, nil
		}
		if wsID != "" {
			if repaired, ok, repairErr := r.resolveStoredIDs(ctx, wsID, projectID, projectSlug); repairErr == nil && ok {
				_, _ = r.Save(cwd, repaired)
				r.warn("Repaired drifted target mapping to canonical format: " + repaired.Canonical())
				return &repaired, nil
			}
		}
		if fallback != nil {
			return fallback, nil
		}
		return nil, resolveErr
	}
	if wsID != "" && projectSlug != "" {
		resolved, ok, resolveErr := r.resolveStoredIDs(ctx, wsID, projectID, projectSlug)
		if resolveErr == nil && ok {
			_, _ = r.Save(cwd, resolved)
			r.warn("Migrated workspace target to canonical format: " + resolved.Canonical())
			return &resolved, nil
		}
		if fallback != nil {
			return fallback, nil
		}
		if resolveErr != nil {
			return nil, resolveErr
		}
		return nil, errors.New("Stored project target is no longer accessible. Run `recallstack project use`.")
	}
	legacy := stringValue(cfg["project"])
	if legacy != "" {
		resolved, resolveErr := r.resolveLegacy(ctx, legacy)
		if resolveErr != nil {
			return nil, resolveErr
		}
		_, _ = r.Save(cwd, resolved)
		r.warn("Migrated legacy target to canonical format: " + resolved.Canonical())
		return &resolved, nil
	}
	return nil, nil
}

func (r *Resolver) resolveCanonical(ctx context.Context, wsSlug, projectSlug string) (Resolved, error) {
	workspaces, err := r.Backend.ListWorkspaces(ctx)
	if err != nil {
		return Resolved{}, err
	}
	for _, ws := range workspaces {
		if ws.Slug == wsSlug {
			if out, ok, err := r.projectInWorkspace(ctx, ws, projectSlug); err != nil {
				return Resolved{}, err
			} else if ok {
				return out, nil
			}
			return Resolved{}, fmt.Errorf("Project %q not found in workspace %q.", projectSlug, ws.Slug)
		}
	}
	return Resolved{}, fmt.Errorf("Workspace %q not found. Run `recallstack project use` to choose from accessible workspaces.", wsSlug)
}

func (r *Resolver) projectInWorkspace(ctx context.Context, ws Workspace, token string) (Resolved, bool, error) {
	projects, err := r.Backend.ListProjects(ctx, ws.ID)
	if err != nil {
		return Resolved{}, false, err
	}
	for _, p := range projects {
		if p.Slug == token || p.ID == token {
			return combine(ws, p), true, nil
		}
	}
	return Resolved{}, false, nil
}

func (r *Resolver) resolveStoredIDs(ctx context.Context, wsID, projectID, projectSlug string) (Resolved, bool, error) {
	workspaces, err := r.Backend.ListWorkspaces(ctx)
	if err != nil {
		return Resolved{}, false, err
	}
	for _, ws := range workspaces {
		if ws.ID == wsID {
			projects, err := r.Backend.ListProjects(ctx, ws.ID)
			if err != nil {
				return Resolved{}, false, err
			}
			for _, p := range projects {
				if (projectID != "" && p.ID == projectID) || p.Slug == projectSlug {
					return combine(ws, p), true, nil
				}
			}
			return Resolved{}, false, nil
		}
	}
	return Resolved{}, false, nil
}

func (r *Resolver) resolveLegacy(ctx context.Context, value string) (Resolved, error) {
	parts := strings.SplitN(strings.TrimSpace(value), "/", 2)
	workspaces, err := r.Backend.ListWorkspaces(ctx)
	if err != nil {
		return Resolved{}, err
	}
	if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != "" {
		for _, ws := range workspaces {
			if ws.ID == strings.TrimSpace(parts[0]) || ws.Slug == strings.TrimSpace(parts[0]) {
				if out, ok, err := r.projectInWorkspace(ctx, ws, strings.TrimSpace(parts[1])); err != nil {
					return Resolved{}, err
				} else if ok {
					return out, nil
				}
				return Resolved{}, fmt.Errorf("Legacy target %q could not be resolved. Project was not found in workspace %q.", value, ws.Slug)
			}
		}
		return Resolved{}, fmt.Errorf("Workspace %q not found. Run `recallstack workspace list`.", strings.TrimSpace(parts[0]))
	}
	var matches []Resolved
	for _, ws := range workspaces {
		if out, ok, err := r.projectInWorkspace(ctx, ws, strings.TrimSpace(value)); err != nil {
			return Resolved{}, err
		} else if ok {
			matches = append(matches, out)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return Resolved{}, fmt.Errorf("Legacy target %q could not be resolved. Run `recallstack project use` to set a new default.", value)
	}
	choices := make([]string, len(matches))
	for i := range matches {
		choices[i] = matches[i].Canonical()
	}
	return Resolved{}, fmt.Errorf("Legacy target %q is ambiguous. Matching targets: %s. Run `recallstack project use <workspaceSlug/projectSlug>`.", value, strings.Join(choices, ", "))
}

func combine(w Workspace, p Project) Resolved {
	return Resolved{w.ID, w.Slug, w.Name, p.ID, p.Slug, p.Name, p.Mode}
}
func stringValue(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }
func storedFallback(cfg map[string]any) *Resolved {
	wsID, projectSlug := stringValue(cfg["workspace_id"]), stringValue(cfg["project_slug"])
	if wsID == "" || projectSlug == "" {
		return nil
	}
	mode := Mode(stringValue(cfg["project_mode"]))
	if mode != ModeGlobal && mode != ModeProject {
		if projectSlug == "global" {
			mode = ModeGlobal
		} else {
			mode = ModeProject
		}
	}
	wsSlug := stringValue(cfg["workspace_slug"])
	if wsSlug == "" {
		wsSlug = wsID
	}
	wsName := stringValue(cfg["workspace_name"])
	if wsName == "" {
		wsName = wsSlug
	}
	pID := stringValue(cfg["project_id"])
	if pID == "" {
		pID = projectSlug
	}
	pName := stringValue(cfg["project_name"])
	if pName == "" {
		pName = projectSlug
	}
	return &Resolved{wsID, wsSlug, wsName, pID, projectSlug, pName, mode}
}
func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil || out == nil {
		return nil, fmt.Errorf("Failed to parse JSON file at %s. Fix the file and retry.", path)
	}
	return out, nil
}
func writeObject(path string, value map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func (r *Resolver) warn(message string) {
	if r.Warn != nil {
		fmt.Fprintln(r.Warn, message)
	}
}
