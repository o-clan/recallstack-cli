package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
)

func configuredStore(t *testing.T, baseURL string) (*config.Store, string) {
	t.Helper()
	t.Setenv("RECALLSTACK_TOKEN_STORE", "file")
	home := t.TempDir()
	cwd := t.TempDir()
	store := &config.Store{HomeDir: home, GOOS: "linux"}
	if err := store.SaveBaseURLOverride(cwd, baseURL); err != nil {
		t.Fatal(err)
	}
	return store, cwd
}

func TestAuthenticatedJSONRecoversRefreshRotationRace(t *testing.T) {
	var store *config.Store
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/me":
			if r.Header.Get("Authorization") == "Bearer access-new" {
				_, _ = w.Write([]byte(`{"id":"user-1"}`))
				return
			}
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
		case "/v1/auth/token/refresh":
			go func() {
				time.Sleep(100 * time.Millisecond)
				if err := store.UpdateProfile(baseURL, config.ProfilePatch{AccessToken: config.String("access-new"), RefreshToken: config.String("refresh-new")}); err != nil {
					t.Error(err)
				}
			}()
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":{"code":"REFRESH_RACE_DETECTED"}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	baseURL = server.URL
	store, cwd := configuredStore(t, baseURL)
	if err := store.UpdateProfile(baseURL, config.ProfilePatch{AccessToken: config.String("access-old"), RefreshToken: config.String("refresh-old")}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, cwd, "recallstack-cli/test")
	var out struct {
		ID string `json:"id"`
	}
	if err := manager.DoJSON(context.Background(), nil, "/v1/auth/me", api.RequestOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "user-1" {
		t.Fatalf("unexpected response: %+v", out)
	}
	cfg, _ := store.Load(cwd)
	if cfg.AccessToken != "access-new" || cfg.RefreshToken != "refresh-new" {
		t.Fatalf("new tokens were lost: %+v", cfg)
	}
}

func TestSuccessfulRefreshCannotOverwriteNewerLogin(t *testing.T) {
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/v1/auth/token/refresh" {
			close(refreshStarted)
			<-releaseRefresh
			_, _ = w.Write([]byte(`{"access_token":"stale-access","refresh_token":"stale-refresh"}`))
			return
		}
		if r.Header.Get("Authorization") == "Bearer login-access" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
	}))
	defer server.Close()
	store, cwd := configuredStore(t, server.URL)
	if err := store.UpdateProfile(server.URL, config.ProfilePatch{AccessToken: config.String("old-access"), RefreshToken: config.String("old-refresh")}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, cwd, "test")
	result := make(chan error, 1)
	go func() {
		var out map[string]any
		result <- manager.DoJSON(context.Background(), nil, "/resource", api.RequestOptions{}, &out)
	}()
	<-refreshStarted
	if err := store.UpdateProfile(server.URL, config.ProfilePatch{AccessToken: config.String("login-access"), RefreshToken: config.String("login-refresh")}); err != nil {
		t.Fatal(err)
	}
	close(releaseRefresh)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.Load(cwd)
	if cfg.AccessToken != "login-access" || cfg.RefreshToken != "login-refresh" {
		t.Fatalf("refresh overwrote login: %+v", cfg)
	}
}

func TestAuthenticatedJSONRefreshesAndFallsBackToAPIKey(t *testing.T) {
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		auth := r.Header.Get("Authorization")
		requests = append(requests, r.URL.Path+" "+auth)
		if r.URL.Path == "/v1/auth/token/refresh" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
			return
		}
		if auth == "Bearer api-good" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
	}))
	defer server.Close()
	store, cwd := configuredStore(t, server.URL)
	if err := store.UpdateProfile(server.URL, config.ProfilePatch{AccessToken: config.String("expired"), RefreshToken: config.String("expired-refresh"), APIKey: config.String("api-good")}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, cwd, "test")
	var out map[string]any
	if err := manager.DoJSON(context.Background(), nil, "/resource", api.RequestOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Fatalf("unexpected response: %#v", out)
	}
	if len(requests) != 3 {
		t.Fatalf("unexpected requests: %#v", requests)
	}
	cfg, _ := store.Load(cwd)
	if cfg.AccessToken != "" || cfg.RefreshToken != "" || cfg.APIKey != "api-good" {
		t.Fatalf("expired JWT chain not cleared safely: %+v", cfg)
	}
}
