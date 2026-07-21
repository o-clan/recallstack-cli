package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/auth"
	"github.com/o-clan/recallstack-cli/internal/config"
	"github.com/o-clan/recallstack-cli/internal/target"
)

type runtimeServices struct {
	deps     Dependencies
	cwd      string
	store    *config.Store
	config   config.Config
	auth     *auth.Manager
	targets  *target.Resolver
	clientID string
	traceID  string
	spanID   string
}

func newRuntimeServices(deps Dependencies) (*runtimeServices, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	store := config.NewStore()
	cfg, err := store.Load(cwd)
	if err != nil {
		return nil, err
	}
	clientID := "recallstack-cli/" + deps.Version
	authManager := auth.NewManager(store, cwd, clientID)
	traceHeaders, traceID, spanID, _ := api.NewTraceContext()
	authManager.Headers = traceHeaders
	runtime := &runtimeServices{
		deps:     deps,
		cwd:      cwd,
		store:    store,
		config:   cfg,
		auth:     authManager,
		clientID: clientID,
		traceID:  traceID,
		spanID:   spanID,
	}
	runtime.targets = &target.Resolver{Backend: targetBackend{auth: authManager, cfg: &runtime.config}, Warn: deps.Stderr}
	return runtime, nil
}

func (r *runtimeServices) reloadConfig() error {
	cfg, err := r.store.Load(r.cwd)
	if err != nil {
		return err
	}
	r.config = cfg
	return nil
}

type targetBackend struct {
	auth *auth.Manager
	cfg  *config.Config
}

func (backend targetBackend) ListWorkspaces(ctx context.Context) ([]target.Workspace, error) {
	var response struct {
		Items []struct {
			Workspace struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
				Name string `json:"name"`
			} `json:"workspace"`
		} `json:"items"`
	}
	if err := backend.auth.DoJSON(ctx, backend.cfg, "/v1/workspaces", api.RequestOptions{}, &response); err != nil {
		return nil, err
	}
	workspaces := make([]target.Workspace, 0, len(response.Items))
	for _, item := range response.Items {
		if item.Workspace.ID == "" || item.Workspace.Slug == "" || item.Workspace.Name == "" {
			continue
		}
		workspaces = append(workspaces, target.Workspace{
			ID: item.Workspace.ID, Slug: item.Workspace.Slug, Name: item.Workspace.Name,
		})
	}
	return workspaces, nil
}

func (backend targetBackend) ListProjects(ctx context.Context, workspaceID string) ([]target.Project, error) {
	var response struct {
		Items []target.Project `json:"items"`
	}
	if err := backend.auth.DoJSON(ctx, backend.cfg, "/v1/projects", api.RequestOptions{WorkspaceID: workspaceID}, &response); err != nil {
		return nil, err
	}
	return response.Items, nil
}

func (r *runtimeServices) requireTargetBackend() error {
	if r.targets == nil || r.targets.Backend == nil {
		return fmt.Errorf("target resolver is unavailable")
	}
	return nil
}
