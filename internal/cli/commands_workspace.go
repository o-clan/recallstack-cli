package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
	"github.com/o-clan/recallstack-cli/internal/target"
)

type workspaceMembership struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	IsActive bool   `json:"isActive"`
}

func (r *runtimeServices) runWorkspace(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	ctx := context.Background()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("workspace list does not accept arguments")
		}
		memberships, err := r.listWorkspaceMemberships(ctx)
		if err != nil {
			return err
		}
		configured := configuredWorkspace(r.config)
		items := make([]map[string]any, 0, len(memberships))
		for _, item := range memberships {
			items = append(items, map[string]any{
				"id": item.ID, "slug": item.Slug, "name": item.Name, "kind": item.Kind,
				"role": item.Role, "status": item.Status, "isActive": item.IsActive,
				"isConfigured": configured != nil && ((configured.ID != "" && configured.ID == item.ID) || (configured.Slug != "" && configured.Slug == item.Slug)),
			})
		}
		return writeJSON(r.deps.Stdout, map[string]any{"items": items})
	case "use":
		if len(args) > 2 {
			return fmt.Errorf("workspace use accepts at most one workspace")
		}
		memberships, err := r.listWorkspaceMemberships(ctx)
		if err != nil {
			return err
		}
		var selected workspaceMembership
		if len(args) == 2 {
			token := strings.TrimSpace(args[1])
			found := false
			for _, item := range memberships {
				if item.ID == token || item.Slug == token {
					selected, found = item, true
					break
				}
			}
			if !found {
				return fmt.Errorf("workspace %q not found. Run `recallstack workspace list`", token)
			}
		} else {
			if len(memberships) == 0 {
				return fmt.Errorf("no accessible workspaces found")
			}
			configured := configuredWorkspace(r.config)
			defaultIndex := 0
			for index, item := range memberships {
				if configured != nil && ((configured.ID != "" && configured.ID == item.ID) || (configured.Slug != "" && configured.Slug == item.Slug)) {
					defaultIndex = index
					break
				}
				if configured == nil && item.IsActive {
					defaultIndex = index
				}
			}
			for index, item := range memberships {
				marker := " "
				if index == defaultIndex {
					marker = "*"
				}
				fmt.Fprintf(r.deps.Stdout, " %s [%d] %s (%s)\n", marker, index+1, item.Name, item.Slug)
			}
			answer, promptErr := promptLine(r.deps.Stdin, r.deps.Stdout, fmt.Sprintf("Choose workspace [%d]: ", defaultIndex+1))
			if promptErr != nil {
				return fmt.Errorf("workspace selection requires TTY input. Use `recallstack workspace use <workspaceSlug|workspaceId>` in non-interactive mode")
			}
			selected = memberships[defaultIndex]
			if answer != "" {
				matched := false
				for index, item := range memberships {
					if answer == fmt.Sprint(index+1) || answer == item.ID || answer == item.Slug {
						selected, matched = item, true
						break
					}
				}
				if !matched {
					return fmt.Errorf("invalid workspace selection %q", answer)
				}
			}
		}
		previous := configuredWorkspace(r.config)
		if err := r.store.UpdateProfile(r.config.EffectiveBaseURL, config.ProfilePatch{
			ActiveWorkspaceID: config.String(selected.ID), ActiveWorkspaceSlug: config.String(selected.Slug), ActiveWorkspaceName: config.String(selected.Name),
		}); err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, map[string]any{
			"workspace":          map[string]any{"id": selected.ID, "slug": selected.Slug, "name": selected.Name},
			"previous_workspace": workspaceJSON(previous),
		})
	case "show":
		if len(args) != 1 {
			return fmt.Errorf("workspace show does not accept arguments")
		}
		configured := configuredWorkspace(r.config)
		active, err := r.resolveConfiguredWorkspace(ctx)
		response := map[string]any{"workspace": workspaceJSON(active), "configured_workspace": workspaceJSON(configured)}
		if err != nil {
			response["workspace_error"] = err.Error()
		}
		return writeJSON(r.deps.Stdout, response)
	case "clear":
		if len(args) != 1 {
			return fmt.Errorf("workspace clear does not accept arguments")
		}
		previous := configuredWorkspace(r.config)
		if err := r.store.UpdateProfile(r.config.EffectiveBaseURL, config.ProfilePatch{
			ActiveWorkspaceID: config.String(""), ActiveWorkspaceSlug: config.String(""), ActiveWorkspaceName: config.String(""),
		}); err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, map[string]any{"cleared": true, "previous_workspace": workspaceJSON(previous)})
	default:
		return fmt.Errorf("unknown workspace command %q", args[0])
	}
}

func (r *runtimeServices) listWorkspaceMemberships(ctx context.Context) ([]workspaceMembership, error) {
	var response struct {
		ActiveWorkspaceID string `json:"activeWorkspaceId"`
		Items             []struct {
			Workspace struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"workspace"`
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := r.auth.DoJSON(ctx, &r.config, "/v1/workspaces", api.RequestOptions{}, &response); err != nil {
		return nil, err
	}
	items := make([]workspaceMembership, 0, len(response.Items))
	for _, item := range response.Items {
		if item.Workspace.ID == "" || item.Workspace.Slug == "" || item.Workspace.Name == "" {
			continue
		}
		items = append(items, workspaceMembership{
			ID: item.Workspace.ID, Slug: item.Workspace.Slug, Name: item.Workspace.Name,
			Kind: choose(item.Workspace.Kind != "", item.Workspace.Kind, "UNKNOWN"),
			Role: choose(item.Role != "", item.Role, "UNKNOWN"), Status: choose(item.Status != "", item.Status, "UNKNOWN"),
			IsActive: item.Workspace.ID == response.ActiveWorkspaceID,
		})
	}
	return items, nil
}

func asWorkspace(value target.Workspace) *target.Workspace { return &value }
