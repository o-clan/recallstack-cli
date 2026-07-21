package synthesis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Target string

const (
	Codex   Target = "codex"
	Claude  Target = "claude"
	Copilot Target = "copilot"
)

type Result struct {
	Answer       string   `json:"answer"`
	PrimaryIDs   []string `json:"primary_ids"`
	ContextIDs   []string `json:"context_ids"`
	Caveats      []string `json:"caveats"`
	WorkerTarget Target   `json:"worker_target"`
	WorkerModel  *string  `json:"worker_model"`
}

type Command struct {
	Name  string
	Args  []string
	Dir   string
	Env   []string
	Input []byte
}
type CommandResult struct {
	Status         int
	Stdout, Stderr []byte
}
type Runner interface {
	Run(context.Context, Command) (CommandResult, error)
	Available(string) bool
}
type ExecRunner struct{}

func (ExecRunner) Available(name string) bool { _, err := exec.LookPath(name); return err == nil }
func (ExecRunner) Run(ctx context.Context, input Command) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, input.Name, input.Args...)
	cmd.Dir = input.Dir
	cmd.Env = input.Env
	cmd.Stdin = bytes.NewReader(input.Input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			status = exit.ExitCode()
		} else {
			return CommandResult{}, err
		}
	}
	return CommandResult{status, stdout.Bytes(), stderr.Bytes()}, nil
}

type Service struct {
	Runner  Runner
	HomeDir string
	TempDir string
}
type Options struct {
	CWD, Query                string
	Retrieval                 any
	WorkerTarget, WorkerModel string
}

func (s *Service) Synthesize(ctx context.Context, options Options) (Result, error) {
	target, err := s.ResolveTarget(options.CWD, options.WorkerTarget)
	if err != nil {
		return Result{}, err
	}
	if !s.runner().Available(string(target)) {
		return Result{}, fmt.Errorf("The %s worker is not available on PATH.", target)
	}
	model := strings.TrimSpace(options.WorkerModel)
	if model == "" {
		model, err = s.ConfiguredModel(options.CWD, target)
		if err != nil {
			return Result{}, err
		}
	}
	var base Result
	switch target {
	case Codex:
		base, err = s.codex(ctx, options.Query, options.Retrieval, model)
	case Claude:
		base, err = s.claude(ctx, options.CWD, options.Query, options.Retrieval, model)
	default:
		base, err = s.copilot(ctx, options.Query, options.Retrieval, model)
	}
	if err != nil {
		return Result{}, err
	}
	base.WorkerTarget = target
	if model != "" {
		base.WorkerModel = &model
	}
	return base, nil
}

func (s *Service) ResolveTarget(cwd, explicit string) (Target, error) {
	if value := strings.TrimSpace(explicit); value != "" {
		target := Target(value)
		if target != Codex && target != Claude && target != Copilot {
			return "", errors.New(`Invalid --worker-target. Use "codex", "claude", or "copilot".`)
		}
		return target, nil
	}
	global, err := readSettings(filepath.Join(s.home(), ".recallstack", "agent-settings.json"))
	if err != nil {
		return "", err
	}
	local, err := readSettings(filepath.Join(cwd, ".recallstack", "agent-settings.json"))
	if err != nil {
		return "", err
	}
	settings := mergeSettings(global, local)
	for _, target := range []Target{Codex, Claude, Copilot} {
		if strings.TrimSpace(settings[string(target)].WorkerModel) != "" {
			return target, nil
		}
	}
	for _, target := range []Target{Codex, Claude, Copilot} {
		if s.runner().Available(string(target)) {
			return target, nil
		}
	}
	return "", errors.New("No local synthesis worker is available. Install codex, claude, or copilot, or pass --worker-target.")
}

func (s *Service) ConfiguredModel(cwd string, target Target) (string, error) {
	local, err := readSettings(filepath.Join(cwd, ".recallstack", "agent-settings.json"))
	if err != nil {
		return "", err
	}
	if model := strings.TrimSpace(local[string(target)].WorkerModel); model != "" {
		return model, nil
	}
	global, err := readSettings(filepath.Join(s.home(), ".recallstack", "agent-settings.json"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(global[string(target)].WorkerModel), nil
}

func Prompt(query string, retrieval any) string {
	schema, _ := json.Marshal(Schema())
	payload, _ := json.MarshalIndent(retrieval, "", "  ")
	return strings.Join([]string{"You are Recallstack's local memory-query synthesis worker.", "Return valid JSON matching the provided schema exactly.", "Use only the retrieval payload below.", "Write a concise answer to the user's query.", "Treat synthesis as a hint layered on top of evidence, not as proof.", "If evidence is partial or conflicting, say so directly in the answer.", "Never invent memory ids, implementation details, or conclusions that are not supported by the payload.", "Put the strongest ids you relied on in primary_ids/context_ids.", "", "QUERY", query, "", "SCHEMA", string(schema), "", "RETRIEVAL PAYLOAD", string(payload)}, "\n")
}
func Schema() map[string]any {
	array := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"answer": map[string]any{"type": "string"}, "primary_ids": array, "context_ids": array, "caveats": array}, "required": []string{"answer", "primary_ids", "context_ids", "caveats"}}
}

func (s *Service) codex(ctx context.Context, query string, retrieval any, model string) (Result, error) {
	dir, err := os.MkdirTemp(s.temp(), "recallstack-query-synth-codex-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	schemaPath, outputPath := filepath.Join(dir, "schema.json"), filepath.Join(dir, "output.json")
	schema, _ := json.MarshalIndent(Schema(), "", "  ")
	if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
		return Result{}, err
	}
	args := []string{"exec", "--ephemeral", "--disable", "codex_hooks", "--skip-git-repo-check", "-C", dir, "--output-schema", schemaPath, "--output-last-message", outputPath, "-"}
	if model != "" {
		args = append([]string{"exec", "--model", model}, args[1:]...)
	}
	res, err := s.runner().Run(ctx, Command{"codex", args, dir, hookEnv(), []byte(Prompt(query, retrieval))})
	if err != nil {
		return Result{}, err
	}
	data, readErr := os.ReadFile(outputPath)
	if res.Status != 0 || readErr != nil {
		return Result{}, fmt.Errorf("Codex synthesis failed.\n%s", failureText(res))
	}
	out, ok := parse(data)
	if !ok {
		return Result{}, fmt.Errorf("Failed to parse JSON file at %s. Fix the file and retry.", outputPath)
	}
	return out, nil
}
func (s *Service) claude(ctx context.Context, cwd, query string, retrieval any, model string) (Result, error) {
	schema, _ := json.Marshal(Schema())
	args := []string{"-p", "--output-format", "json", "--json-schema", string(schema), "--tools", "", "--no-session-persistence", "--append-system-prompt", "You are a strict JSON synthesis pass for Recallstack memory query output."}
	if model != "" {
		args = append([]string{"-p", "--model", model}, args[1:]...)
	}
	res, err := s.runner().Run(ctx, Command{"claude", args, cwd, hookEnv(), []byte(Prompt(query, retrieval))})
	if err != nil {
		return Result{}, err
	}
	if res.Status != 0 {
		return Result{}, fmt.Errorf("Claude synthesis failed.\n%s", failureText(res))
	}
	if out, ok := parseClaude(res.Stdout); ok {
		return out, nil
	}
	return Result{}, errors.New("Claude synthesis returned unreadable output.")
}
func (s *Service) copilot(ctx context.Context, query string, retrieval any, model string) (Result, error) {
	dir, err := os.MkdirTemp(s.temp(), "recallstack-query-synth-copilot-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	args := []string{"--config-dir", dir, "--no-custom-instructions", "--allow-all-tools", "--output-format", "text", "--silent", "-p", Prompt(query, retrieval)}
	if model != "" {
		args = append([]string{"--model", model}, args...)
	}
	res, err := s.runner().Run(ctx, Command{"copilot", args, dir, hookEnv(), nil})
	if err != nil {
		return Result{}, err
	}
	if res.Status != 0 {
		return Result{}, fmt.Errorf("Copilot synthesis failed.\n%s", failureText(res))
	}
	if out, ok := parse(res.Stdout); ok {
		return out, nil
	}
	return Result{}, errors.New("Copilot synthesis returned unreadable output.")
}

func parse(data []byte) (Result, bool) {
	var raw struct {
		Answer     string `json:"answer"`
		PrimaryIDs []any  `json:"primary_ids"`
		ContextIDs []any  `json:"context_ids"`
		Caveats    []any  `json:"caveats"`
	}
	if json.Unmarshal(data, &raw) != nil || strings.TrimSpace(raw.Answer) == "" {
		return Result{}, false
	}
	return Result{Answer: strings.TrimSpace(raw.Answer), PrimaryIDs: compact(raw.PrimaryIDs), ContextIDs: compact(raw.ContextIDs), Caveats: compact(raw.Caveats)}, true
}
func parseClaude(data []byte) (Result, bool) {
	if direct, ok := parse(data); ok {
		return direct, true
	}
	var wrapper map[string]any
	if json.Unmarshal(data, &wrapper) != nil {
		return Result{}, false
	}
	value, hasStringResult := wrapper["result"].(string)
	if !hasStringResult {
		parts := []string{}
		content, _ := wrapper["content"].([]any)
		for _, raw := range content {
			item, _ := raw.(map[string]any)
			typeName, _ := item["type"].(string)
			text, _ := item["text"].(string)
			if typeName == "text" && text != "" {
				parts = append(parts, text)
			}
		}
		value = strings.Join(parts, "\n")
	}
	return parse([]byte(value))
}
func compact(values []any) []string {
	out := []string{}
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
}
func failureText(res CommandResult) string {
	value := res.Stderr
	if len(value) == 0 {
		value = res.Stdout
	}
	return strings.TrimSpace(string(value))
}
func hookEnv() []string { return append(os.Environ(), "RECALLSTACK_HOOK_ACTIVE=1") }
func (s *Service) runner() Runner {
	if s.Runner != nil {
		return s.Runner
	}
	return ExecRunner{}
}
func (s *Service) home() string {
	if s.HomeDir != "" {
		return s.HomeDir
	}
	value, _ := os.UserHomeDir()
	return value
}
func (s *Service) temp() string {
	if s.TempDir != "" {
		return s.TempDir
	}
	return os.TempDir()
}

type setting struct {
	WorkerModel string `json:"worker_model"`
}

func readSettings(path string) (map[string]setting, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]setting{}, nil
		}
		return nil, err
	}
	var value map[string]setting
	if json.Unmarshal(data, &value) != nil {
		return nil, fmt.Errorf("Failed to parse JSON file at %s. Fix the file and retry.", path)
	}
	return value, nil
}
func mergeSettings(global, local map[string]setting) map[string]setting {
	out := map[string]setting{}
	for key, value := range global {
		out[key] = value
	}
	for key, value := range local {
		out[key] = value
	}
	return out
}
