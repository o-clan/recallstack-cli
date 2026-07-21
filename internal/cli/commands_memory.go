package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/auth"
	"github.com/o-clan/recallstack-cli/internal/config"
	"github.com/o-clan/recallstack-cli/internal/memory"
	"github.com/o-clan/recallstack-cli/internal/pending"
	"github.com/o-clan/recallstack-cli/internal/proof"
	"github.com/o-clan/recallstack-cli/internal/synthesis"
)

type memoryClient struct {
	auth  *auth.Manager
	cfg   *config.Config
	queue *pending.Store
	cwd   string
}

type queuedWriteError struct{ output map[string]any }

func (e *queuedWriteError) Error() string { return "memory write queued locally" }

func (client memoryClient) Request(ctx context.Context, method, path, workspaceID string, body any) (map[string]any, error) {
	var out map[string]any
	err := client.auth.DoJSON(ctx, client.cfg, path, api.RequestOptions{Method: method, Body: body, WorkspaceID: workspaceID}, &out)
	if err == nil {
		if out == nil {
			out = map[string]any{}
		}
		return out, nil
	}
	if !pending.IsAuthRequired(err) || (path != "/v1/memory/events" && path != "/v1/memory/sources") {
		return nil, err
	}
	requestBody, ok := body.(map[string]any)
	if !ok {
		return nil, err
	}
	kind := pending.KindEvent
	message := "Memory saved locally and will sync after login."
	if path == "/v1/memory/sources" {
		kind = pending.KindSource
		message = "Durable source saved locally and will sync after login."
	}
	project, _ := requestBody["project_id"].(string)
	queued, queueErr := client.queue.Queue(pending.QueueInput{
		Kind: kind, BaseURL: client.cfg.EffectiveBaseURL, WorkspaceID: workspaceID, ProjectToken: project,
		Body: requestBody, Origin: "cli", Cwd: client.cwd, QueueReason: pending.AuthQueueReason(err),
		LastError: &pending.LastError{Message: err.Error(), Code: auth.ErrorCode(err)},
	})
	if queueErr != nil {
		return nil, queueErr
	}
	return nil, &queuedWriteError{output: map[string]any{
		"status": "queued_local", "queued_local": true, "duplicate": queued.Duplicate,
		"queue_id": queued.Record.ID, "sync_status": "pending_login", "project_id": project,
		"message": message, "memory_footer": fmt.Sprintf("Memory updated: pending local sync (reason=AUTH_REQUIRED, queue=%s)", queued.Record.ID),
	}}
}

func (r *runtimeServices) memoryService() *memory.Service {
	return &memory.Service{
		Client:    memoryClient{auth: r.auth, cfg: &r.config, queue: pending.NewStore(r.store.HomeDir), cwd: r.cwd},
		Targets:   r.targets,
		Proof:     &proof.Tracker{},
		Synthesis: &synthesis.Service{HomeDir: r.store.HomeDir},
	}
}

func (r *runtimeServices) runMemory(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("memory requires ingest, query, or source")
	}
	switch args[0] {
	case "ingest":
		return r.runMemoryIngest(args[1:])
	case "query":
		return r.runMemoryQuery(args[1:])
	case "source":
		return r.runMemorySource(args[1:])
	default:
		return fmt.Errorf("unknown memory command %q", args[0])
	}
}

func (r *runtimeServices) runMemoryIngest(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(
		[]string{"project", "file", "idempotency-key", "metadata", "turn-id", "diff-proof-mode", "diff-proof"},
		[]string{"stdin", "force-event", "disable-diff-proof"},
	))
	if err != nil {
		return err
	}
	if err := memory.ValidateInput(parsed.values["file"], parsed.bools["stdin"], "memory ingest"); err != nil {
		return err
	}
	content, err := r.memoryInput(parsed)
	if err != nil {
		return err
	}
	var metadata map[string]any
	if parsed.present["metadata"] {
		metadata, err = memory.ParseObject(parsed.values["metadata"], "metadata")
		if err != nil {
			return err
		}
	}
	var diff map[string]any
	if parsed.present["diff-proof"] {
		diff, err = memory.ParseObject(parsed.values["diff-proof"], "diff-proof")
		if err != nil {
			return err
		}
	}
	out, err := r.memoryService().Ingest(context.Background(), memory.IngestOptions{
		CWD: r.cwd, Project: parsed.values["project"], Content: content, Metadata: metadata,
		IdempotencyKey: parsed.values["idempotency-key"], ForceEvent: parsed.bools["force-event"],
		TurnID: parsed.values["turn-id"], DisableDiffProof: parsed.bools["disable-diff-proof"],
		DisableDiffProofSet: parsed.present["disable-diff-proof"], DiffProofMode: parsed.values["diff-proof-mode"],
		DiffProofModeSet: parsed.present["diff-proof-mode"], DiffProof: diff,
	})
	if err != nil {
		var queued *queuedWriteError
		if errors.As(err, &queued) {
			return writeJSON(r.deps.Stdout, queued.output)
		}
		return err
	}
	return writeJSON(r.deps.Stdout, out)
}

func (r *runtimeServices) runMemoryQuery(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(
		[]string{"query", "project", "lens", "mode", "worker-target", "worker-model", "turn-id"},
		[]string{"synthesize", "json", "verbose", "synthesize-only", "synthesize-with-caveats"},
	))
	if err != nil {
		return err
	}
	query := strings.TrimSpace(parsed.values["query"])
	if query == "" {
		return fmt.Errorf("required option --query not specified")
	}
	options := memory.QueryOptions{
		CWD: r.cwd, Project: parsed.values["project"], Query: query, Lens: parsed.values["lens"], Mode: parsed.values["mode"],
		TurnID: parsed.values["turn-id"], JSON: parsed.bools["json"], Verbose: parsed.bools["verbose"],
		Synthesize: parsed.bools["synthesize"], SynthesizeOnly: parsed.bools["synthesize-only"],
		SynthesizeWithCaveats: parsed.bools["synthesize-with-caveats"], WorkerTarget: parsed.values["worker-target"], WorkerModel: parsed.values["worker-model"],
	}
	result, err := r.memoryService().Query(context.Background(), options)
	if err != nil {
		return err
	}
	formatted := memory.FormatQuery(result, options, time.Now())
	formatted = memory.FormatStructured(formatted, terminalWriter(r.deps.Stdout) && !options.JSON)
	_, err = fmt.Fprintln(r.deps.Stdout, formatted)
	return err
}

func (r *runtimeServices) runMemorySource(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("memory source requires ingest, query, or get")
	}
	service := r.memoryService()
	switch args[0] {
	case "ingest":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"title", "source-type", "project", "uri", "file", "metadata"}, []string{"stdin"}))
		if err != nil {
			return err
		}
		if strings.TrimSpace(parsed.values["title"]) == "" {
			return fmt.Errorf("required option --title not specified")
		}
		if strings.TrimSpace(parsed.values["source-type"]) == "" {
			return fmt.Errorf("required option --source-type not specified")
		}
		if err := memory.ValidateInput(parsed.values["file"], parsed.bools["stdin"], "memory source ingest"); err != nil {
			return err
		}
		content, err := r.memoryInput(parsed)
		if err != nil {
			return err
		}
		var metadata map[string]any
		if parsed.present["metadata"] {
			metadata, err = memory.ParseObject(parsed.values["metadata"], "metadata")
			if err != nil {
				return err
			}
		}
		out, err := service.IngestSource(context.Background(), memory.SourceIngestOptions{CWD: r.cwd, Project: parsed.values["project"], Title: parsed.values["title"], SourceType: parsed.values["source-type"], URI: parsed.values["uri"], Content: content, Metadata: metadata})
		if err != nil {
			var queued *queuedWriteError
			if errors.As(err, &queued) {
				return writeJSON(r.deps.Stdout, queued.output)
			}
			return err
		}
		return writeJSON(r.deps.Stdout, out)
	case "query":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"query", "project", "k", "max-chunks-per-source"}, nil))
		if err != nil {
			return err
		}
		if strings.TrimSpace(parsed.values["query"]) == "" {
			return fmt.Errorf("required option --query not specified")
		}
		out, err := service.QuerySources(context.Background(), memory.SourceQueryOptions{CWD: r.cwd, Project: parsed.values["project"], Query: parsed.values["query"], K: number(parsed.values["k"]), MaxChunksPerSource: number(parsed.values["max-chunks-per-source"])})
		if err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, out)
	case "get":
		parsed, err := parseOptions(args[1:], newOptionSpec([]string{"source-id", "project"}, nil))
		if err != nil {
			return err
		}
		if strings.TrimSpace(parsed.values["source-id"]) == "" {
			return fmt.Errorf("required option --source-id not specified")
		}
		out, err := service.GetSource(context.Background(), r.cwd, parsed.values["project"], parsed.values["source-id"])
		if err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, out)
	default:
		return fmt.Errorf("unknown memory source command %q", args[0])
	}
}

func (r *runtimeServices) memoryInput(parsed parsedOptions) (string, error) {
	if parsed.bools["stdin"] {
		return readAll(r.deps.Stdin)
	}
	raw, err := os.ReadFile(parsed.values["file"])
	return string(raw), err
}

func number(value string) float64 {
	parsed, _ := strconv.ParseFloat(value, 64)
	return parsed
}

func terminalWriter(writer any) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
