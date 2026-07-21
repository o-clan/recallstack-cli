package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
)

var ErrNotAuthenticated = errors.New("Not authenticated. Run `recallstack login`.")

type ConfigStore interface {
	Load(cwd string) (config.Config, error)
	UpdateProfile(baseURL string, patch config.ProfilePatch) error
	UpdateProfileIfCurrent(baseURL string, expected, patch config.ProfilePatch) (bool, error)
}

type Manager struct {
	Store      ConfigStore
	Cwd        string
	ClientID   string
	HTTPClient *http.Client
	Headers    map[string]string
}

func NewManager(store ConfigStore, cwd, clientID string) *Manager {
	return &Manager{Store: store, Cwd: cwd, ClientID: clientID}
}

func (m *Manager) client(baseURL string) *api.Client {
	client := api.NewClient(baseURL, m.ClientID)
	client.DefaultHeaders = m.Headers
	if m.HTTPClient != nil {
		client.HTTPClient = m.HTTPClient
	}
	return client
}

func (m *Manager) runtimeConfig(cfg *config.Config) (config.Config, error) {
	if cfg != nil {
		return *cfg, nil
	}
	if m.Store == nil {
		return config.Config{}, errors.New("auth config store is required")
	}
	return m.Store.Load(m.Cwd)
}

func (m *Manager) LoginWithCode(ctx context.Context, code string, cfg *config.Config) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("Sign-in code is required.")
	}
	runtimeConfig, err := m.runtimeConfig(cfg)
	if err != nil {
		return err
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	err = m.client(runtimeConfig.EffectiveBaseURL).DoJSON(ctx, "/v1/auth/cli/code/exchange", api.RequestOptions{
		Method: http.MethodPost,
		Body:   map[string]any{"code": code},
	}, &response)
	if err != nil {
		return err
	}
	return m.Store.UpdateProfile(runtimeConfig.EffectiveBaseURL, config.ProfilePatch{
		AccessToken: config.String(response.AccessToken), RefreshToken: config.String(response.RefreshToken), APIKey: config.String(""),
	})
}

type LogoutResult struct {
	RemoteRevoked bool
	Warning       string
}

func (m *Manager) Logout(ctx context.Context, cfg *config.Config) (LogoutResult, error) {
	runtimeConfig, err := m.runtimeConfig(cfg)
	if err != nil {
		return LogoutResult{}, err
	}
	result := LogoutResult{}
	if runtimeConfig.AccessToken != "" {
		body := map[string]any{}
		if runtimeConfig.RefreshToken != "" {
			body["refresh_token"] = runtimeConfig.RefreshToken
		}
		var response any
		err := m.client(runtimeConfig.EffectiveBaseURL).DoJSON(ctx, "/v1/auth/logout", api.RequestOptions{
			Method: http.MethodPost, Body: body, Token: runtimeConfig.AccessToken,
		}, &response)
		if err != nil {
			result.Warning = "Remote session revoke failed: " + err.Error()
		} else {
			result.RemoteRevoked = true
		}
	} else if runtimeConfig.RefreshToken != "" {
		result.Warning = "Remote session revoke skipped: missing access token."
	}
	err = m.Store.UpdateProfile(runtimeConfig.EffectiveBaseURL, config.ProfilePatch{
		AccessToken: config.String(""), RefreshToken: config.String(""), APIKey: config.String(""),
	})
	return result, err
}

func (m *Manager) DoJSON(ctx context.Context, cfg *config.Config, path string, options api.RequestOptions, out any) error {
	runtimeConfig, err := m.runtimeConfig(cfg)
	if err != nil {
		return err
	}
	baseURL := runtimeConfig.EffectiveBaseURL
	client := m.client(baseURL)

	if runtimeConfig.AccessToken != "" {
		err = doWithToken(ctx, client, path, options, runtimeConfig.AccessToken, out)
		if err == nil {
			return nil
		}
		if !isHTTPStatus(err, http.StatusUnauthorized) {
			return err
		}

		if runtimeConfig.RefreshToken != "" {
			refreshed, refreshErr := m.refreshAccessToken(ctx, runtimeConfig, baseURL)
			if refreshErr == nil && refreshed.AccessToken != "" {
				return doWithToken(ctx, client, path, options, refreshed.AccessToken, out)
			}
			if refreshErr == nil {
				refreshErr = ErrNotAuthenticated
			}
			if retryErr, retried := m.retryWithFreshAuth(ctx, baseURL, runtimeConfig, path, options, out); retried {
				return retryErr
			}
			if runtimeConfig.APIKey != "" {
				return doWithToken(ctx, client, path, options, runtimeConfig.APIKey, out)
			}
			return refreshErr
		}

		if runtimeConfig.APIKey != "" {
			return doWithToken(ctx, client, path, options, runtimeConfig.APIKey, out)
		}
		if retryErr, retried := m.retryWithFreshAuth(ctx, baseURL, runtimeConfig, path, options, out); retried {
			return retryErr
		}
		return err
	}

	if runtimeConfig.APIKey != "" {
		return doWithToken(ctx, client, path, options, runtimeConfig.APIKey, out)
	}
	return ErrNotAuthenticated
}

func doWithToken(ctx context.Context, client *api.Client, path string, options api.RequestOptions, token string, out any) error {
	options.Token = token
	return client.DoJSON(ctx, path, options, out)
}

func (m *Manager) refreshAccessToken(ctx context.Context, cfg config.Config, baseURL string) (config.Config, error) {
	if cfg.RefreshToken == "" {
		return config.Config{}, ErrNotAuthenticated
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	err := m.client(baseURL).DoJSON(ctx, "/v1/auth/token/refresh", api.RequestOptions{
		Method: http.MethodPost, Body: map[string]any{"refresh_token": cfg.RefreshToken},
	}, &response)
	if err == nil {
		updated, updateErr := m.Store.UpdateProfileIfCurrent(baseURL,
			config.ProfilePatch{RefreshToken: config.String(cfg.RefreshToken)},
			config.ProfilePatch{AccessToken: config.String(response.AccessToken), RefreshToken: config.String(response.RefreshToken)},
		)
		if updateErr != nil {
			return config.Config{}, updateErr
		}
		if !updated {
			if latest, ok := m.latestConfigForBaseURL(baseURL); ok && configHasNewerAuth(latest, cfg) {
				return latest, nil
			}
			return config.Config{}, errors.New("Authentication changed while refreshing. Retry the command.")
		}
		cfg.AccessToken = response.AccessToken
		cfg.RefreshToken = response.RefreshToken
		return cfg, nil
	}
	if isHTTPCode(err, http.StatusConflict, "REFRESH_RACE_DETECTED") {
		if latest, ok := m.waitForNewerAuth(ctx, baseURL, cfg, time.Second); ok {
			return latest, nil
		}
	}
	if !isHTTPStatus(err, http.StatusUnauthorized) {
		return config.Config{}, err
	}
	if latest, ok := m.latestConfigForBaseURL(baseURL); ok && configHasNewerAuth(latest, cfg) {
		return latest, nil
	}
	_, updateErr := m.Store.UpdateProfileIfCurrent(baseURL,
		config.ProfilePatch{RefreshToken: config.String(cfg.RefreshToken)},
		config.ProfilePatch{AccessToken: config.String(""), RefreshToken: config.String("")},
	)
	if updateErr != nil {
		return config.Config{}, updateErr
	}
	return config.Config{}, errors.New("Authentication expired. Run `recallstack login`.")
}

func (m *Manager) waitForNewerAuth(ctx context.Context, baseURL string, previous config.Config, timeout time.Duration) (config.Config, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if latest, ok := m.latestConfigForBaseURL(baseURL); ok && configHasNewerAuth(latest, previous) {
			return latest, true
		}
		if time.Now().After(deadline) {
			return config.Config{}, false
		}
		select {
		case <-ctx.Done():
			return config.Config{}, false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (m *Manager) retryWithFreshAuth(ctx context.Context, baseURL string, previous config.Config, path string, options api.RequestOptions, out any) (error, bool) {
	latest, ok := m.latestConfigForBaseURL(baseURL)
	if !ok || !configHasNewerAuth(latest, previous) {
		return nil, false
	}
	if latest.AccessToken != "" {
		return doWithToken(ctx, m.client(baseURL), path, options, latest.AccessToken, out), true
	}
	if latest.RefreshToken != "" {
		refreshed, err := m.refreshAccessToken(ctx, latest, baseURL)
		if err != nil {
			return err, true
		}
		if refreshed.AccessToken != "" {
			return doWithToken(ctx, m.client(baseURL), path, options, refreshed.AccessToken, out), true
		}
	}
	return ErrNotAuthenticated, true
}

func (m *Manager) latestConfigForBaseURL(baseURL string) (config.Config, bool) {
	latest, err := m.Store.Load(m.Cwd)
	return latest, err == nil && latest.EffectiveBaseURL == baseURL
}

func configHasNewerAuth(latest, previous config.Config) bool {
	return (latest.AccessToken != "" && latest.AccessToken != previous.AccessToken) ||
		(latest.RefreshToken != "" && latest.RefreshToken != previous.RefreshToken)
}

func isHTTPStatus(err error, status int) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == status
}

func isHTTPCode(err error, status int, code string) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == status && httpErr.Code == code
}

func ErrorCode(err error) string {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Code != "" {
		return httpErr.Code
	}
	if err == nil {
		return ""
	}
	return "Error"
}
