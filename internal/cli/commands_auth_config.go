package cli

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
	"github.com/o-clan/recallstack-cli/internal/pending"
	"github.com/o-clan/recallstack-cli/internal/target"
)

func (r *runtimeServices) runAuth(args []string) error {
	ctx := context.Background()
	switch args[0] {
	case "login":
		if len(args) > 2 {
			return fmt.Errorf("login accepts at most one code")
		}
		code := ""
		if len(args) == 2 {
			code = strings.TrimSpace(args[1])
		}
		if code == "" {
			var err error
			code, err = promptLine(r.deps.Stdin, r.deps.Stdout, "Sign-in code: ")
			if err != nil {
				return fmt.Errorf("sign-in code is required; pass it as `recallstack login <code>` in non-interactive mode")
			}
		}
		if err := r.auth.LoginWithCode(ctx, code, &r.config); err != nil {
			return err
		}
		if err := r.reloadConfig(); err != nil {
			return err
		}
		queue := pending.NewStore(r.store.HomeDir)
		replay, err := queue.Flush(ctx, r.config, func(ctx context.Context, record pending.Record, cfg config.Config) error {
			var out any
			return r.auth.DoJSON(ctx, &cfg, record.Endpoint, api.RequestOptions{
				Method: http.MethodPost, Body: record.Body, WorkspaceID: record.WorkspaceID,
			}, &out)
		})
		if err != nil {
			return err
		}
		if replay.Replayed > 0 {
			label := "writes"
			if replay.Replayed == 1 {
				label = "write"
			}
			fmt.Fprintf(r.deps.Stdout, "Replayed %d queued memory %s.\n", replay.Replayed, label)
		}
		if replay.Remaining > 0 {
			label := "writes are"
			if replay.Remaining == 1 {
				label = "write is"
			}
			fmt.Fprintf(r.deps.Stderr, "%d queued memory %s still pending.\n", replay.Remaining, label)
		}
		fmt.Fprintf(r.deps.Stdout, "Login successful for %s. Global auth config: %s\n", r.config.EffectiveBaseURL, r.store.ResolveScope(r.cwd, config.ScopeGlobal).Path)
		return nil
	case "logout":
		if len(args) != 1 {
			return fmt.Errorf("logout does not accept arguments")
		}
		result, err := r.auth.Logout(ctx, &r.config)
		if err != nil {
			return err
		}
		if result.Warning != "" {
			fmt.Fprintln(r.deps.Stderr, result.Warning)
		}
		if result.RemoteRevoked {
			_, err = fmt.Fprintln(r.deps.Stdout, "Logged out. Remote session revoked.")
		} else {
			_, err = fmt.Fprintln(r.deps.Stdout, "Logged out.")
		}
		return err
	case "whoami":
		if len(args) != 1 {
			return fmt.Errorf("whoami does not accept arguments")
		}
		var out any
		if err := r.auth.DoJSON(ctx, &r.config, "/v1/auth/me", api.RequestOptions{}, &out); err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, out)
	default:
		return fmt.Errorf("unknown auth command %q", args[0])
	}
}

func (r *runtimeServices) runConfig(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "where":
		if len(args) != 1 {
			return fmt.Errorf("config where does not accept arguments")
		}
		_, err := fmt.Fprintln(r.deps.Stdout, r.store.Resolve(r.cwd).Path)
		return err
	case "show":
		if len(args) != 1 {
			return fmt.Errorf("config show does not accept arguments")
		}
		return r.showConfig()
	case "base-url":
		parsed, err := parseOptions(args[1:], newOptionSpec(nil, []string{"clear"}))
		if err != nil {
			return err
		}
		if len(parsed.positionals) > 1 {
			return fmt.Errorf("config base-url accepts at most one URL")
		}
		url := ""
		if len(parsed.positionals) == 1 {
			url = parsed.positionals[0]
		}
		if parsed.bools["clear"] && url != "" {
			return fmt.Errorf("provide either a URL or --clear, not both")
		}
		if !parsed.bools["clear"] && url == "" {
			return fmt.Errorf("provide a URL or use --clear")
		}
		if err := r.store.SaveBaseURLOverride(r.cwd, url); err != nil {
			return err
		}
		if err := r.reloadConfig(); err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, map[string]any{
			"base_url_override":  nullable(r.config.BaseURL),
			"default_base_url":   config.DefaultBaseURL,
			"effective_base_url": r.config.EffectiveBaseURL,
			"auth_profile_key":   r.config.EffectiveBaseURL,
			"override_source":    choose(r.config.BaseURL != "", "config.baseUrl", "default"),
		})
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

func (r *runtimeServices) showConfig() error {
	resolved := r.store.Resolve(r.cwd)
	configured := configuredWorkspace(r.config)
	active, activeErr := r.resolveConfiguredWorkspace(context.Background())
	response := map[string]any{
		"config_path":          resolved.Path,
		"config_scope":         resolved.Scope,
		"scope_root":           resolved.ScopeRoot,
		"base_url_override":    nullable(r.config.BaseURL),
		"default_base_url":     config.DefaultBaseURL,
		"effective_base_url":   r.config.EffectiveBaseURL,
		"auth_profile_key":     r.config.EffectiveBaseURL,
		"authenticated":        r.config.AccessToken != "" || r.config.APIKey != "",
		"auth_status":          r.authStatus(),
		"override_source":      choose(r.config.BaseURL != "", "config.baseUrl", "default"),
		"active_workspace":     workspaceJSON(active),
		"configured_workspace": workspaceJSON(configured),
	}
	if activeErr != nil {
		response["active_workspace_error"] = activeErr.Error()
	}
	return writeJSON(r.deps.Stdout, response)
}

func (r *runtimeServices) authStatus() map[string]any {
	queue := pending.NewStore(r.store.HomeDir).List(r.config.EffectiveBaseURL)
	authPattern := regexp.MustCompile(`(?i)AUTH|LOGIN|UNAUTHORIZED|401`)
	authBlocked := 0
	for _, record := range queue {
		if authPattern.MatchString(record.QueueReason) {
			authBlocked++
		}
	}
	authenticated := r.config.AccessToken != "" || r.config.APIKey != ""
	status := "login_required"
	if authenticated {
		status = "authenticated"
	}
	if authBlocked > 0 {
		if authenticated {
			status = "authenticated_with_pending_auth_replay"
		} else {
			status = "login_required_with_pending_replay"
		}
	}
	tokenSource := any(nil)
	if r.config.AccessToken != "" {
		tokenSource = "jwt"
	} else if r.config.APIKey != "" {
		tokenSource = "api_key"
	}
	tokenStorage := r.config.TokenStorage
	if tokenStorage == "" {
		tokenStorage = "file"
	}
	return map[string]any{
		"authenticated": authenticated, "token_source": tokenSource, "token_storage": tokenStorage,
		"status": status, "pending_writes": len(queue), "auth_blocked_pending_writes": authBlocked,
		"login_required": !authenticated || authBlocked > 0,
	}
}

func configuredWorkspace(cfg config.Config) *target.Workspace {
	if cfg.ActiveWorkspaceID == "" && cfg.ActiveWorkspaceSlug == "" {
		return nil
	}
	return &target.Workspace{ID: cfg.ActiveWorkspaceID, Slug: cfg.ActiveWorkspaceSlug, Name: choose(cfg.ActiveWorkspaceName != "", cfg.ActiveWorkspaceName, "Configured workspace")}
}

func (r *runtimeServices) resolveConfiguredWorkspace(ctx context.Context) (*target.Workspace, error) {
	configured := configuredWorkspace(r.config)
	if configured == nil {
		return nil, nil
	}
	workspaces, err := targetBackend{auth: r.auth, cfg: &r.config}.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	for index := range workspaces {
		if (configured.ID != "" && workspaces[index].ID == configured.ID) || (configured.Slug != "" && workspaces[index].Slug == configured.Slug) {
			return &workspaces[index], nil
		}
	}
	return nil, nil
}

func workspaceJSON(workspace *target.Workspace) any {
	if workspace == nil {
		return nil
	}
	return map[string]any{"id": nullable(workspace.ID), "slug": nullable(workspace.Slug), "name": nullable(workspace.Name)}
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func choose[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}
