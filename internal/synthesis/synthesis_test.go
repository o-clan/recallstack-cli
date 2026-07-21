package synthesis

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fakeRunner struct {
	available map[string]bool
	result    CommandResult
	calls     []Command
}

func (f *fakeRunner) Available(name string) bool { return f.available[name] }
func (f *fakeRunner) Run(_ context.Context, cmd Command) (CommandResult, error) {
	f.calls = append(f.calls, cmd)
	if cmd.Name == "codex" {
		for i, arg := range cmd.Args {
			if arg == "--output-last-message" {
				data, _ := json.Marshal(map[string]any{"answer": " yes ", "primary_ids": []string{" a ", ""}, "context_ids": []string{}, "caveats": []string{}})
				_ = os.WriteFile(cmd.Args[i+1], data, 0o600)
			}
		}
	}
	return f.result, nil
}
func TestConfiguredModelSelectsAndRunsCodex(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".recallstack", "agent-settings.json"), []byte(`{"codex":{"worker_model":"gpt-test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{available: map[string]bool{"codex": true}}
	service := Service{Runner: runner, HomeDir: home, TempDir: t.TempDir()}
	got, err := service.Synthesize(context.Background(), Options{CWD: cwd, Query: "q", Retrieval: map[string]any{"x": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Answer != "yes" || got.WorkerModel == nil || *got.WorkerModel != "gpt-test" || len(got.PrimaryIDs) != 1 || got.PrimaryIDs[0] != "a" {
		t.Fatalf("unexpected result: %#v", got)
	}
	if runner.calls[0].Args[1] != "--model" {
		t.Fatalf("model args missing: %#v", runner.calls[0].Args)
	}
}
func TestClaudeWrapperParsing(t *testing.T) {
	wrapped := []byte(`{"content":[{"type":"text","text":"{\"answer\":\"ok\",\"primary_ids\":[],\"context_ids\":[],\"caveats\":[]}"}]}`)
	got, ok := parseClaude(wrapped)
	if !ok || got.Answer != "ok" {
		t.Fatalf("unexpected parse: %#v %v", got, ok)
	}
}
func TestInvalidTarget(t *testing.T) {
	service := Service{Runner: &fakeRunner{available: map[string]bool{}}}
	if _, err := service.ResolveTarget(t.TempDir(), "cursor"); err == nil {
		t.Fatal("expected error")
	}
}
