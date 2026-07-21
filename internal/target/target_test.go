package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	workspaces []Workspace
	projects   map[string][]Project
	err        error
}

func (f fakeBackend) ListWorkspaces(context.Context) ([]Workspace, error) { return f.workspaces, f.err }
func (f fakeBackend) ListProjects(_ context.Context, id string) ([]Project, error) {
	return f.projects[id], f.err
}

func TestResolveAndMigrateLegacyTarget(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(filepath.Join(dir, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(WorkspaceConfigPath(dir), []byte(`{"project":"ws-id/project-id","kept":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings strings.Builder
	r := Resolver{Backend: fakeBackend{workspaces: []Workspace{{ID: "ws-id", Slug: "team", Name: "Team"}}, projects: map[string][]Project{"ws-id": {{ID: "project-id", Slug: "api", Name: "API", Mode: ModeProject}}}}, Warn: &warnings, Now: func() time.Time { return time.Unix(0, 0) }}
	got, err := r.LoadLocal(context.Background(), nested)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Canonical() != "team/api" {
		t.Fatalf("unexpected target: %#v", got)
	}
	if !strings.Contains(warnings.String(), "Migrated legacy target") {
		t.Fatalf("missing warning: %s", warnings.String())
	}
	data, err := os.ReadFile(WorkspaceConfigPath(nested))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"workspace_slug": "team"`) || strings.Contains(text, `"project":`) {
		t.Fatalf("unexpected migration: %s", text)
	}
}

func TestResolveShortProjectAmbiguous(t *testing.T) {
	r := Resolver{Backend: fakeBackend{workspaces: []Workspace{{ID: "1", Slug: "one"}, {ID: "2", Slug: "two"}}, projects: map[string][]Project{"1": {{ID: "a", Slug: "api", Mode: ModeProject}}, "2": {{ID: "b", Slug: "api", Mode: ModeProject}}}}}
	_, err := r.ResolveInput(context.Background(), "api", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous across workspaces: one/api, two/api") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWritableRejectsGlobal(t *testing.T) {
	dir := t.TempDir()
	r := Resolver{Backend: fakeBackend{workspaces: []Workspace{{ID: "1", Slug: "one", Name: "One"}}, projects: map[string][]Project{"1": {{ID: "g", Slug: "global", Name: "Global", Mode: ModeGlobal}}}}}
	if _, err := r.Save(dir, Resolved{"1", "one", "One", "g", "global", "Global", ModeGlobal}); err != nil {
		t.Fatal(err)
	}
	_, err := r.ResolveForCommand(context.Background(), dir, "", true)
	if err == nil || !strings.HasPrefix(err.Error(), "GLOBAL_WRITE_FORBIDDEN") {
		t.Fatalf("unexpected error: %v", err)
	}
}
