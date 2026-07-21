package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type startedProcess struct {
	executable string
	args       []string
	cwd        string
	env        []string
}

type hookHarness struct {
	t       *testing.T
	cwd     string
	now     time.Time
	started []startedProcess
	runs    []struct {
		command string
		args    []string
		options CommandOptions
	}
	responses func(string, []string, CommandOptions) CommandResult
	stdout    bytes.Buffer
	stderr    bytes.Buffer
}

func newHarness(t *testing.T) *hookHarness {
	t.Helper()
	return &hookHarness{t: t, cwd: t.TempDir(), now: time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)}
}

func (h *hookHarness) options(payload map[string]any) Options {
	data, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	return Options{
		Stdin: bytes.NewReader(data), Stdout: &h.stdout, Stderr: &h.stderr, CWD: h.cwd,
		Now: func() time.Time { return h.now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			h.now = h.now.Add(delay)
			return nil
		},
		Run: func(_ context.Context, command string, args []string, options CommandOptions) CommandResult {
			h.runs = append(h.runs, struct {
				command string
				args    []string
				options CommandOptions
			}{command, append([]string(nil), args...), options})
			if h.responses != nil {
				return h.responses(command, args, options)
			}
			return CommandResult{}
		},
		Start: func(executable string, args []string, cwd string, env []string) (int, error) {
			h.started = append(h.started, startedProcess{executable: executable, args: append([]string(nil), args...), cwd: cwd, env: append([]string(nil), env...)})
			return 4321, nil
		},
	}
}

func (h *hookHarness) queuedPath(t *testing.T) string {
	t.Helper()
	if len(h.started) == 0 {
		t.Fatal("expected a queued worker")
	}
	args := h.started[len(h.started)-1].args
	if len(args) != 3 || args[0] != HiddenWorkerCommand() {
		t.Fatalf("unexpected self reexec args: %#v", args)
	}
	return args[2]
}

func workerOptions(base Options) Options { base.Stdin = bytes.NewReader(nil); return base }

func TestRunRejectsUnknownAgent(t *testing.T) {
	h := newHarness(t)
	if err := Run(context.Background(), "unknown", "stop", h.options(nil)); err == nil {
		t.Fatal("expected unsupported agent error")
	}
}

func TestAntigravityAlwaysAllowsMalformedPayload(t *testing.T) {
	h := newHarness(t)
	options := h.options(nil)
	options.Stdin = strings.NewReader("not-json")
	if err := Run(context.Background(), "antigravity", "noop", options); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(h.stdout.String()) != `{"decision":"allow"}` {
		t.Fatalf("unexpected decision: %q", h.stdout.String())
	}
}

func TestCodexSessionStartPreservesStateSchema(t *testing.T) {
	h := newHarness(t)
	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "session-1", "transcript_path": "/tmp/transcript.jsonl", "cwd": h.cwd, "source": "startup"}
	if err := Run(context.Background(), "codex", "session_start", h.options(payload)); err != nil {
		t.Fatal(err)
	}
	state := readJSONMap(sessionPath(stateDir(h.cwd, "codex"), "session-1"))
	for key, expected := range map[string]string{"session_id": "session-1", "transcript_path": "/tmp/transcript.jsonl", "cwd": h.cwd, "source": "startup"} {
		if actual := stringValue(state[key]); actual != expected {
			t.Fatalf("%s: got %q want %q", key, actual, expected)
		}
	}
}

func TestClaudeWorkerUsesExactCompatibleIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	transcript := copyFixture(t, h.cwd, "claude.jsonl")
	payload := map[string]any{"session_id": "claude-session", "transcript_path": transcript, "cwd": h.cwd}
	options := h.options(payload)
	if err := Run(context.Background(), "claude", "stop", options); err != nil {
		t.Fatal(err)
	}
	jobPath := h.queuedPath(t)
	h.responses = successfulProjectAndIngestWithExtractorFailure
	if err := Worker(context.Background(), "claude", jobPath, workerOptions(options)); err != nil {
		t.Fatal(err)
	}
	assertIngest(t, h.runs, "claude-hook-"+sha1Hex("claude-session:assistant-new"), "User intent: Ship the native hook")
	state := readJSONMap(sessionPath(stateDir(h.cwd, "claude"), "claude-session"))
	if stringValue(state["last_ingested_turn_id"]) != "assistant-new" {
		t.Fatalf("unexpected state: %#v", state)
	}
}

func TestCodexWorkerUsesTurnIDForCompatibility(t *testing.T) {
	h := newHarness(t)
	transcript := copyFixture(t, h.cwd, "codex.jsonl")
	payload := map[string]any{"hook_event_name": "Stop", "session_id": "codex-session", "transcript_path": transcript, "cwd": h.cwd}
	options := h.options(payload)
	if err := Run(context.Background(), "codex", "stop", options); err != nil {
		t.Fatal(err)
	}
	jobPath := h.queuedPath(t)
	h.responses = successfulProjectAndIngestWithExtractorFailure
	if err := Worker(context.Background(), "codex", jobPath, workerOptions(options)); err != nil {
		t.Fatal(err)
	}
	assertIngest(t, h.runs, "codex-hook-"+sha1Hex("codex-session:turn-native"), "User intent: Port the hooks to Go")
}

func TestCursorCapturesPairAndDeduplicatesStop(t *testing.T) {
	h := newHarness(t)
	base := map[string]any{"conversation_id": "conversation-1", "generation_id": "generation-1", "workspace_roots": []any{h.cwd}}
	if err := Run(context.Background(), "cursor", "beforeSubmitPrompt", h.options(mergeMap(base, map[string]any{"prompt": "Build native hooks"}))); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), "cursor", "afterAgentResponse", h.options(mergeMap(base, map[string]any{"text": "Native hooks built"}))); err != nil {
		t.Fatal(err)
	}
	options := h.options(base)
	if err := Run(context.Background(), "cursor", "stop", options); err != nil {
		t.Fatal(err)
	}
	jobPath := h.queuedPath(t)
	h.responses = successfulProjectAndIngestWithExtractorFailure
	if err := Worker(context.Background(), "cursor", jobPath, workerOptions(options)); err != nil {
		t.Fatal(err)
	}
	assertIngest(t, h.runs, "cursor-hook-"+sha1Hex("conversation-1:generation-1"), "User intent: Build native hooks")
	started := len(h.started)
	if err := Run(context.Background(), "cursor", "stop", h.options(base)); err != nil {
		t.Fatal(err)
	}
	if len(h.started) != started {
		t.Fatal("duplicate stop queued another worker")
	}
}

func TestCopilotNormalizesVSCodeEventsAndReadsTranscript(t *testing.T) {
	h := newHarness(t)
	transcript := copyFixture(t, h.cwd, "copilot.jsonl")
	replaceInFile(t, transcript, "REPOSITORY_ROOT", h.cwd)
	base := map[string]any{"session_id": "copilot-session", "cwd": h.cwd, "transcript_path": transcript}
	if err := Run(context.Background(), "copilot", "UserPromptSubmit", h.options(mergeMap(base, map[string]any{"prompt": "Port Copilot hooks"}))); err != nil {
		t.Fatal(err)
	}
	options := h.options(base)
	if err := Run(context.Background(), "copilot", "Stop", options); err != nil {
		t.Fatal(err)
	}
	jobPath := h.queuedPath(t)
	h.responses = successfulProjectAndIngestWithExtractorFailure
	if err := Worker(context.Background(), "copilot", jobPath, workerOptions(options)); err != nil {
		t.Fatal(err)
	}
	assertIngest(t, h.runs, "copilot-hook-"+sha1Hex(h.cwd+":message-1"), "User intent: Port Copilot hooks")
}

func TestAntigravityWorkerPreservesAllowAndIdempotencyPrefix(t *testing.T) {
	h := newHarness(t)
	transcript := copyFixture(t, h.cwd, "antigravity.jsonl")
	payload := map[string]any{"conversationId": "ag-session", "transcriptPath": transcript, "workspacePaths": []any{h.cwd}}
	options := h.options(payload)
	if err := Run(context.Background(), "antigravity", "stop", options); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(h.stdout.String()) != `{"decision":"allow"}` {
		t.Fatalf("unexpected decision: %q", h.stdout.String())
	}
	jobPath := h.queuedPath(t)
	h.responses = successfulProjectAndIngestWithExtractorFailure
	if err := Worker(context.Background(), "antigravity", jobPath, workerOptions(options)); err != nil {
		t.Fatal(err)
	}
	dedupe := sha1Hex("ag-session\n" + transcript + "\n1\n2\nPort Antigravity hooks\nAntigravity hooks are native")
	expectedKey := "antigravity-hook-" + sha1Hex(h.cwd+":"+dedupe)
	found := false
	for _, call := range h.runs {
		if len(call.args) > 0 && call.args[0] == "memory" {
			key := argumentAfter(call.args, "--idempotency-key")
			if key != expectedKey {
				t.Fatalf("key got %q want %q", key, expectedKey)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected ingest call")
	}
}

func TestRetryReexecAndSuccessfulReplay(t *testing.T) {
	h := newHarness(t)
	options := h.options(nil)
	rt, err := newRuntime(options)
	if err != nil {
		t.Fatal(err)
	}
	state := stateDir(h.cwd, "cursor")
	result := CommandResult{Status: 503, Stderr: "service unavailable"}
	record := map[string]any{"cwd": h.cwd, "target": "workspace/project", "summary": "summary", "metadata": map[string]any{}, "idempotency_key": "cursor-hook-key", "conversation_id": "c", "generation_id": "g"}
	if err := queueRetry(rt, "cursor", state, h.cwd, record, result); err != nil {
		t.Fatal(err)
	}
	started := h.started[len(h.started)-1]
	if started.args[0] != HiddenRetryCommand() || started.args[1] != "cursor" {
		t.Fatalf("unexpected retry args: %#v", started.args)
	}
	h.responses = successfulProjectAndIngestWithExtractorFailure
	if err := Retry(context.Background(), "cursor", started.args[2], workerOptions(options)); err != nil {
		t.Fatal(err)
	}
	stateValue := readJSONMap(conversationPath(state, "c"))
	if stringValue(stateValue["last_ingested_generation_id"]) != "g" {
		t.Fatalf("retry did not mark state: %#v", stateValue)
	}
}

func TestRetryClassificationAndBackoffCompatibility(t *testing.T) {
	for _, result := range []CommandResult{
		{Status: 1, Stderr: "HTTP 429 too many requests"},
		{Status: 1, Stderr: "503 service unavailable"},
		{Status: 1, Stderr: "connection refused"},
	} {
		if !isRetryable(result) {
			t.Fatalf("expected retryable result: %#v", result)
		}
	}
	if isRetryable(CommandResult{Status: 1, Stderr: "HTTP 400 invalid request"}) {
		t.Fatal("permanent failure classified as retryable")
	}
	policy := retryPolicy{Base: 30 * time.Second, Max: 15 * time.Minute}
	if policy.delay(1) != 30*time.Second || policy.delay(2) != time.Minute || policy.delay(20) != 15*time.Minute {
		t.Fatal("backoff no longer matches legacy exponential schedule")
	}
}

func TestThinSummaryOmitsSubstantialOnlyFields(t *testing.T) {
	summary := formatSummary(extractedTurn{TurnKind: "thin", UserIntent: "Proceed", AgentIntent: "Implement", KeyActions: []string{"changed code"}, Tradeoffs: []string{"cost"}, Outcome: "Started", OpenQuestions: []string{"which model"}})
	if summary != "User intent: Proceed\nOutcome: Started" {
		t.Fatalf("unexpected thin summary: %q", summary)
	}
}

func TestCopilotRetryReadsLegacyNestedTurnState(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(stateDir(h.cwd, "copilot"), "retry", "legacy.json")
	if err := writeJSON(path, map[string]any{
		"cwd": h.cwd, "target": "workspace/project", "summary": "summary", "metadata": map[string]any{},
		"idempotency_key": "copilot-hook-key", "session_key": "legacy-session", "repo_root": h.cwd,
		"turn": map[string]any{"dedupe_key": "legacy-dedupe"}, "last_prompt": "Legacy prompt",
		"created_at": h.now.Add(-time.Minute).Format(timeFormat), "expires_at": h.now.Add(time.Hour).Format(timeFormat),
		"next_attempt_at": h.now.Add(-time.Second).Format(timeFormat), "attempt_count": 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.responses = successfulProjectAndIngestWithExtractorFailure
	if err := Retry(context.Background(), "copilot", path, workerOptions(h.options(nil))); err != nil {
		t.Fatal(err)
	}
	state := readJSONMap(sessionPath(stateDir(h.cwd, "copilot"), "legacy-session"))
	if stringValue(state["last_ingested_turn_id"]) != "legacy-dedupe" {
		t.Fatalf("legacy retry state was not preserved: %#v", state)
	}
}

func successfulProjectAndIngestWithExtractorFailure(_ string, args []string, _ CommandOptions) CommandResult {
	if len(args) > 0 && args[0] == "project" {
		return CommandResult{Status: 0, Stdout: `{"target":"workspace/project","project_slug":"project"}`}
	}
	if len(args) > 1 && args[0] == "memory" && args[1] == "ingest" {
		return CommandResult{Status: 0, Stdout: `{}`}
	}
	return CommandResult{Status: 1, Stderr: "worker unavailable"}
}

func assertIngest(t *testing.T, calls []struct {
	command string
	args    []string
	options CommandOptions
}, expectedKey, content string) {
	t.Helper()
	for _, call := range calls {
		if len(call.args) > 1 && call.args[0] == "memory" && call.args[1] == "ingest" {
			if actual := argumentAfter(call.args, "--idempotency-key"); actual != expectedKey {
				t.Fatalf("key got %q want %q", actual, expectedKey)
			}
			if !strings.Contains(call.options.Input, content) {
				t.Fatalf("ingest content %q missing %q", call.options.Input, content)
			}
			return
		}
	}
	t.Fatal("expected memory ingest call")
}

func argumentAfter(args []string, flag string) string {
	for index, value := range args {
		if value == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func copyFixture(t *testing.T, cwd, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func replaceInFile(t *testing.T, path, old, replacement string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), old, replacement)), 0o644); err != nil {
		t.Fatal(err)
	}
}
