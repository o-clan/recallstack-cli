package cli

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/target"
)

func (r *runtimeServices) runProject(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	ctx := context.Background()
	switch args[0] {
	case "show":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"cwd"}, nil))
		if err != nil {
			return err
		}
		cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
		found, _, err := target.FindWorkspaceConfig(cwd)
		if err != nil {
			return err
		}
		if found == "" {
			found = target.WorkspaceConfigPath(cwd)
		}
		selected, err := r.targets.LoadLocal(ctx, cwd)
		if err != nil {
			return err
		}
		response := map[string]any{"cwd": cwd, "workspace_config_path": found, "project": nil, "target": nil, "workspace_id": nil, "workspace_slug": nil, "project_id": nil, "project_slug": nil}
		if selected != nil {
			response["project"], response["target"] = selected.Canonical(), selected.Canonical()
			response["workspace_id"], response["workspace_slug"] = selected.WorkspaceID, selected.WorkspaceSlug
			response["project_id"], response["project_slug"] = selected.ProjectID, selected.ProjectSlug
		}
		return writeJSON(r.deps.Stdout, response)
	case "where":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"cwd"}, nil))
		if err != nil {
			return err
		}
		cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
		found, _, err := target.FindWorkspaceConfig(cwd)
		if err != nil {
			return err
		}
		if found == "" {
			found = target.WorkspaceConfigPath(cwd)
		}
		_, err = fmt.Fprintln(r.deps.Stdout, found)
		return err
	case "clear":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"cwd"}, nil))
		if err != nil {
			return err
		}
		cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
		path, err := r.targets.Clear(cwd)
		if err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, map[string]any{"cwd": cwd, "workspace_config_path": path, "cleared": true})
	case "create":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"name", "description", "workspace"}, nil))
		if err != nil {
			return err
		}
		name := strings.TrimSpace(parsed.values["name"])
		if name == "" {
			return fmt.Errorf("required option --name not specified")
		}
		workspace, err := r.workspaceForCommand(ctx, parsed.values["workspace"])
		if err != nil {
			return err
		}
		if workspace == nil {
			return fmt.Errorf("workspace context missing. Run `recallstack workspace use <workspaceSlug|workspaceId>` (or `recallstack project use`) first, or pass `--workspace <workspaceSlug|workspaceId>`")
		}
		body := map[string]any{"name": name}
		if parsed.values["description"] != "" {
			body["description"] = parsed.values["description"]
		}
		var out any
		if err := r.auth.DoJSON(ctx, &r.config, "/v1/projects", api.RequestOptions{Method: http.MethodPost, Body: body, WorkspaceID: workspace.ID}, &out); err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, out)
	case "list":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"workspace"}, nil))
		if err != nil {
			return err
		}
		workspaces, err := r.workspacesForProjectList(ctx, parsed.values["workspace"])
		if err != nil {
			return err
		}
		backend := targetBackend{auth: r.auth, cfg: &r.config}
		items := []map[string]any{}
		for _, workspace := range workspaces {
			projects, err := backend.ListProjects(ctx, workspace.ID)
			if err != nil {
				return err
			}
			for _, project := range projects {
				items = append(items, map[string]any{
					"workspace_id": workspace.ID, "workspace_slug": workspace.Slug, "workspace_name": workspace.Name,
					"id": project.ID, "slug": project.Slug, "name": project.Name, "mode": project.Mode,
					"target": workspace.Slug + "/" + project.Slug,
				})
			}
		}
		return writeJSON(r.deps.Stdout, map[string]any{"items": items})
	case "use", "set":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"cwd"}, nil))
		if err != nil {
			return err
		}
		if len(parsed.positionals) > 1 {
			return fmt.Errorf("project %s accepts at most one project", args[0])
		}
		cwd := choose(parsed.values["cwd"] != "", parsed.values["cwd"], r.cwd)
		return r.setProject(ctx, cwd, parsed.positionals)
	default:
		return fmt.Errorf("unknown project command %q", args[0])
	}
}

func (r *runtimeServices) setProject(ctx context.Context, cwd string, positional []string) error {
	previous, _ := r.targets.LoadLocal(ctx, cwd)
	var selected target.Resolved
	if len(positional) == 1 {
		configured, _ := r.resolveConfiguredWorkspace(ctx)
		value, err := r.targets.ResolveInput(ctx, positional[0], cwd, configured)
		if err != nil {
			return err
		}
		selected = value
	} else {
		choices, err := r.writableTargets(ctx, true)
		if err != nil {
			return err
		}
		if len(choices) == 0 {
			return fmt.Errorf("no writable projects found across accessible workspaces")
		}
		defaultIndex := 0
		if previous != nil {
			for index := range choices {
				if choices[index].Canonical() == previous.Canonical() {
					defaultIndex = index
					break
				}
			}
		}
		for index, item := range choices {
			fmt.Fprintf(r.deps.Stdout, " [%d] %s (%s)\n", index+1, item.Canonical(), item.ProjectName)
		}
		answer, err := promptLine(r.deps.Stdin, r.deps.Stdout, fmt.Sprintf("Choose project [%d]: ", defaultIndex+1))
		if err != nil {
			return fmt.Errorf("project selection requires TTY input. Use `recallstack project use <projectSlug|workspaceSlug/projectSlug>` in non-interactive mode")
		}
		selected = choices[defaultIndex]
		if answer != "" {
			found := false
			for index, item := range choices {
				if answer == fmt.Sprint(index+1) || answer == item.Canonical() {
					selected, found = item, true
					break
				}
			}
			if !found {
				return fmt.Errorf("invalid project selection %q", answer)
			}
		}
	}
	if selected.Mode == target.ModeGlobal {
		return fmt.Errorf("GLOBAL_WRITE_FORBIDDEN: raw memory writes cannot target global. Select a non-global project")
	}
	path, err := r.targets.Save(cwd, selected)
	if err != nil {
		return err
	}
	response := map[string]any{
		"cwd": cwd, "workspace_config_path": path, "project": selected.Canonical(), "target": selected.Canonical(),
		"workspace_id": selected.WorkspaceID, "workspace_slug": selected.WorkspaceSlug,
		"project_id": selected.ProjectID, "project_slug": selected.ProjectSlug,
		"previous_project": nil, "previous_target": nil,
	}
	if previous != nil {
		response["previous_project"], response["previous_target"] = previous.Canonical(), previous.Canonical()
	}
	return writeJSON(r.deps.Stdout, response)
}

func (r *runtimeServices) workspaceForCommand(ctx context.Context, token string) (*target.Workspace, error) {
	if token != "" {
		workspaces, err := targetBackend{auth: r.auth, cfg: &r.config}.ListWorkspaces(ctx)
		if err != nil {
			return nil, err
		}
		for index := range workspaces {
			if workspaces[index].ID == token || workspaces[index].Slug == token {
				return &workspaces[index], nil
			}
		}
		return nil, fmt.Errorf("workspace %q not found. Run `recallstack workspace list`", token)
	}
	if local, _ := r.targets.LoadLocal(ctx, r.cwd); local != nil {
		return &target.Workspace{ID: local.WorkspaceID, Slug: local.WorkspaceSlug, Name: local.WorkspaceName}, nil
	}
	return r.resolveConfiguredWorkspace(ctx)
}

func (r *runtimeServices) workspacesForProjectList(ctx context.Context, token string) ([]target.Workspace, error) {
	if token != "" {
		workspace, err := r.workspaceForCommand(ctx, token)
		if err != nil {
			return nil, err
		}
		return []target.Workspace{*workspace}, nil
	}
	if workspace, _ := r.workspaceForCommand(ctx, ""); workspace != nil {
		return []target.Workspace{*workspace}, nil
	}
	return targetBackend{auth: r.auth, cfg: &r.config}.ListWorkspaces(ctx)
}

func (r *runtimeServices) writableTargets(ctx context.Context, preferContext bool) ([]target.Resolved, error) {
	var workspaces []target.Workspace
	var err error
	if preferContext {
		workspaces, err = r.workspacesForProjectList(ctx, "")
	} else {
		workspaces, err = targetBackend{auth: r.auth, cfg: &r.config}.ListWorkspaces(ctx)
	}
	if err != nil {
		return nil, err
	}
	backend := targetBackend{auth: r.auth, cfg: &r.config}
	var out []target.Resolved
	for _, workspace := range workspaces {
		projects, err := backend.ListProjects(ctx, workspace.ID)
		if err != nil {
			return nil, err
		}
		for _, project := range projects {
			if project.Mode != target.ModeProject {
				continue
			}
			out = append(out, target.Resolved{WorkspaceID: workspace.ID, WorkspaceSlug: workspace.Slug, WorkspaceName: workspace.Name, ProjectID: project.ID, ProjectSlug: project.Slug, ProjectName: project.Name, Mode: project.Mode})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Canonical() < out[j].Canonical() })
	return out, nil
}
