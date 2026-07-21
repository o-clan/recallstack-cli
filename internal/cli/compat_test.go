package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTestCLI(t *testing.T, args []string, input string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(args, Dependencies{Version: "1.2.0", Stdin: strings.NewReader(input), Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

func TestAgentModelCompatibility(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RECALLSTACK_TOKEN_STORE", "file")
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	stdout, stderr, err := runTestCLI(t, []string{"agent", "model", "codex", "gpt-5.4"}, "")
	if err != nil {
		t.Fatalf("set model: %v: %s", err, stderr)
	}
	var set map[string]any
	if err := json.Unmarshal([]byte(stdout), &set); err != nil {
		t.Fatal(err)
	}
	if set["scope"] != "local" || set["target"] != "codex" || set["worker_model"] != "gpt-5.4" {
		t.Fatalf("unexpected set output: %#v", set)
	}
	model, _ := set["model"].(map[string]any)
	if model["worker_model"] != "gpt-5.4" || model["worker_model_source"] != "local" || model["change_command"] != "recallstack agent model codex <model>" {
		t.Fatalf("unexpected model status: %#v", model)
	}

	stdout, _, err = runTestCLI(t, []string{"agent", "model"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var listed map[string]any
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatal(err)
	}
	models, _ := listed["models"].(map[string]any)
	codex, _ := models["codex"].(map[string]any)
	if codex["worker_model"] != "gpt-5.4" {
		t.Fatalf("unexpected model list: %#v", listed)
	}

	stdout, _, err = runTestCLI(t, []string{"agent", "status"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	codex, _ = status["codex"].(map[string]any)
	if codex["worker_model"] != "gpt-5.4" || codex["worker_model_source"] != "local" || codex["worker_model_change_command"] != "recallstack agent model codex <model>" {
		t.Fatalf("unexpected agent status: %#v", status)
	}

	stdout, _, err = runTestCLI(t, []string{"agent", "model", "codex", "--clear"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var cleared map[string]any
	if err := json.Unmarshal([]byte(stdout), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared["cleared"] != true {
		t.Fatalf("unexpected clear output: %#v", cleared)
	}
}

func TestAuthBlockedIngestQueuesAndLoginReplays(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RECALLSTACK_TOKEN_STORE", "file")
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	var eventAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		switch request.URL.Path {
		case "/v1/workspaces", "/v1/projects":
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, `{"error":{"code":"NOT_FOUND"}}`)
		case "/v1/memory/events":
			eventAttempts++
			if request.Header.Get("authorization") != "Bearer access-good" {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(writer, `{"error":{"code":"UNAUTHORIZED"}}`)
				return
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(writer, `{"memory":{"id":"mem-1"},"deduped":false,"validation":{"applied":false}}`)
		case "/v1/auth/token/refresh":
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":{"code":"UNAUTHORIZED"}}`)
		case "/v1/auth/cli/code/exchange":
			_, _ = io.WriteString(writer, `{"access_token":"access-good","refresh_token":"refresh-good"}`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := filepath.Join(workspace, ".recallstack")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(stateDir, "workspace.json"), map[string]any{
		"workspace_id": "ws-smoke", "workspace_slug": "smoke-ws", "workspace_name": "Smoke Workspace",
		"project_id": "proj-smoke-id", "project_slug": "smoke-project", "project_name": "Smoke Project", "project_mode": "PROJECT",
	})
	writeTestJSON(t, filepath.Join(home, ".recallstack", "config.json"), map[string]any{
		"baseUrl": server.URL,
		"profiles": map[string]any{server.URL: map[string]any{
			"accessToken": "expired-token", "refreshToken": "expired-refresh",
			"activeWorkspaceId": "ws-smoke", "activeWorkspaceSlug": "smoke-ws", "activeWorkspaceName": "Smoke Workspace",
		}},
	})

	stdout, stderr, err := runTestCLI(t, []string{"memory", "ingest", "--stdin"}, "queued during auth loss\n")
	if err != nil {
		t.Fatalf("ingest: %v: %s", err, stderr)
	}
	var queued map[string]any
	if err := json.Unmarshal([]byte(stdout), &queued); err != nil {
		t.Fatal(err)
	}
	if queued["status"] != "queued_local" || queued["sync_status"] != "pending_login" {
		t.Fatalf("unexpected queue output: %#v", queued)
	}

	stdout, stderr, err = runTestCLI(t, []string{"login", "SMOKE-CODE"}, "")
	if err != nil {
		t.Fatalf("login: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Replayed 1 queued memory write.") || !strings.Contains(stdout, "Login successful") {
		t.Fatalf("unexpected login output: %q", stdout)
	}
	if eventAttempts != 2 {
		t.Fatalf("event attempts = %d, want 2", eventAttempts)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
