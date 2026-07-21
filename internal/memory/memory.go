package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/o-clan/recallstack-cli/internal/proof"
	"github.com/o-clan/recallstack-cli/internal/synthesis"
	"github.com/o-clan/recallstack-cli/internal/target"
)

type Client interface {
	Request(context.Context, string, string, string, any) (map[string]any, error)
}
type TargetResolver interface {
	ResolveForCommand(context.Context, string, string, bool) (target.Resolved, error)
}
type ProofTracker interface {
	Start(string, string) error
	Close(string, string) proof.CloseResult
}
type Synthesizer interface {
	Synthesize(context.Context, synthesis.Options) (synthesis.Result, error)
}

type Service struct {
	Client    Client
	Targets   TargetResolver
	Proof     ProofTracker
	Synthesis Synthesizer
	Now       func() time.Time
}

type IngestOptions struct {
	CWD                 string
	Project             string
	Content             string
	Metadata            map[string]any
	IdempotencyKey      string
	ForceEvent          bool
	TurnID              string
	DisableDiffProof    bool
	DisableDiffProofSet bool
	DiffProofMode       string
	DiffProofModeSet    bool
	DiffProof           map[string]any
}

type QueryOptions struct {
	CWD, Project, Query, Lens, Mode, TurnID                          string
	JSON, Verbose, Synthesize, SynthesizeOnly, SynthesizeWithCaveats bool
	WorkerTarget, WorkerModel                                        string
}
type QueryResult struct {
	Response  map[string]any
	Synthesis *synthesis.Result
}
type SourceIngestOptions struct {
	CWD, Project, Title, SourceType, URI, Content string
	Metadata                                      map[string]any
}
type SourceQueryOptions struct {
	CWD, Project, Query   string
	K, MaxChunksPerSource float64
}

func (s *Service) Ingest(ctx context.Context, options IngestOptions) (map[string]any, error) {
	if options.DiffProofMode == "" {
		options.DiffProofMode = "advisory"
	}
	if options.DiffProofMode != "advisory" && options.DiffProofMode != "strict" {
		return nil, errors.New(`Invalid --diff-proof-mode. Use "advisory" or "strict".`)
	}
	if options.DisableDiffProofSet && options.TurnID == "" {
		return nil, errors.New("--disable-diff-proof requires --turn-id.")
	}
	if options.DisableDiffProofSet && options.DiffProofModeSet {
		return nil, errors.New("Cannot combine --disable-diff-proof with --diff-proof-mode.")
	}
	if options.DisableDiffProofSet && options.DiffProof != nil {
		return nil, errors.New("Cannot combine --disable-diff-proof with --diff-proof.")
	}
	if options.DiffProofModeSet && options.DiffProof != nil {
		return nil, errors.New("Cannot combine --diff-proof-mode with --diff-proof; include mode inside the --diff-proof JSON payload.")
	}
	if options.DiffProofModeSet && options.TurnID == "" && options.DiffProof == nil {
		return nil, errors.New("--diff-proof-mode requires --turn-id or --diff-proof.")
	}
	selected, err := s.Targets.ResolveForCommand(ctx, options.CWD, options.Project, true)
	if err != nil {
		return nil, err
	}
	metadata := copyMap(options.Metadata)
	if options.TurnID != "" {
		metadata["turn_id"] = options.TurnID
	}
	diffProof := options.DiffProof
	if diffProof == nil && options.TurnID != "" && !options.DisableDiffProof {
		closed := s.Proof.Close(options.CWD, options.TurnID)
		var generated any
		if closed.Proof != nil {
			generated = closed.Proof
		}
		var hint any
		if closed.Proof != nil && proof.Bytes(closed.Proof) > proof.MaxBytes {
			if options.DiffProofMode == "strict" {
				return nil, fmt.Errorf("Diff proof exceeds %d bytes in strict mode.", proof.MaxBytes)
			}
			hint = "proof_too_large"
			generated = nil
		}
		diffProof = map[string]any{"enabled": true, "mode": options.DiffProofMode, "turn_id": options.TurnID, "claim_text": options.Content}
		if hint != nil {
			diffProof["validation_hint"] = hint
		}
		if generated != nil {
			diffProof["proof"] = generated
		}
	}
	body := map[string]any{"project_id": selected.ProjectSlug, "content": options.Content, "metadata": metadata, "force_event": options.ForceEvent}
	if diffProof != nil {
		body["diff_proof"] = diffProof
	}
	key := options.IdempotencyKey
	if key == "" {
		key = "cli-" + strconv.FormatInt(s.now().UnixMilli(), 10)
	}
	body["idempotency_key"] = key
	out, err := s.Client.Request(ctx, "POST", "/v1/memory/events", selected.WorkspaceID, body)
	if err != nil {
		return nil, err
	}
	validation, _ := out["validation"].(map[string]any)
	footer := "Memory updated: yes (status=not_applied)"
	if applied, _ := validation["applied"].(bool); applied {
		footer = fmt.Sprintf("Memory updated: yes (status=%v, confidence=%v, proof=%v)", validation["status"], validation["confidence_after"], valueOr(validation["proof_fingerprint"], "none"))
	}
	out["memory_footer"] = footer
	return out, nil
}

func (s *Service) Query(ctx context.Context, options QueryOptions) (QueryResult, error) {
	selected, err := s.Targets.ResolveForCommand(ctx, options.CWD, options.Project, false)
	if err != nil {
		return QueryResult{}, err
	}
	if options.TurnID != "" {
		if err := s.Proof.Start(options.CWD, options.TurnID); err != nil {
			return QueryResult{}, err
		}
	}
	if options.Verbose && !options.JSON {
		return QueryResult{}, errors.New("--verbose requires --json.")
	}
	if options.SynthesizeOnly && options.SynthesizeWithCaveats {
		return QueryResult{}, errors.New("Cannot combine --synthesize-only and --synthesize-with-caveats.")
	}
	if options.JSON && (options.SynthesizeOnly || options.SynthesizeWithCaveats) {
		return QueryResult{}, errors.New("Cannot combine --json with synthesis display filters.")
	}
	if options.Lens == "" {
		options.Lens = "mixed"
	}
	if options.Mode == "" {
		options.Mode = "standard"
	}
	synthMode := options.Synthesize || options.WorkerTarget != "" || options.WorkerModel != "" || options.SynthesizeOnly || options.SynthesizeWithCaveats
	format := "structured"
	if synthMode || options.JSON {
		format = "json"
	}
	body := map[string]any{"project_id": selected.ProjectSlug, "query": options.Query, "lens": options.Lens, "mode": options.Mode, "k": 10, "verbose": options.JSON && options.Verbose, "response_format": format}
	out, err := s.Client.Request(ctx, "POST", "/v1/memory/retrieve", selected.WorkspaceID, body)
	if err != nil {
		return QueryResult{}, err
	}
	result := QueryResult{Response: out}
	if synthMode {
		value, err := s.Synthesis.Synthesize(ctx, synthesis.Options{CWD: options.CWD, Query: options.Query, Retrieval: out, WorkerTarget: options.WorkerTarget, WorkerModel: options.WorkerModel})
		if err != nil {
			return QueryResult{}, err
		}
		result.Synthesis = &value
	}
	return result, nil
}

func (s *Service) IngestSource(ctx context.Context, options SourceIngestOptions) (map[string]any, error) {
	selected, err := s.Targets.ResolveForCommand(ctx, options.CWD, options.Project, true)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"project_id": selected.ProjectSlug, "title": options.Title, "source_type": options.SourceType, "content": options.Content}
	if options.URI != "" {
		body["uri"] = options.URI
	}
	if options.Metadata != nil {
		body["metadata"] = options.Metadata
	}
	return s.Client.Request(ctx, "POST", "/v1/memory/sources", selected.WorkspaceID, body)
}
func (s *Service) QuerySources(ctx context.Context, options SourceQueryOptions) (map[string]any, error) {
	selected, err := s.Targets.ResolveForCommand(ctx, options.CWD, options.Project, false)
	if err != nil {
		return nil, err
	}
	k := options.K
	if k == 0 {
		k = 5
	}
	max := options.MaxChunksPerSource
	if max == 0 {
		max = 2
	}
	return s.Client.Request(ctx, "POST", "/v1/memory/sources/query", selected.WorkspaceID, map[string]any{"project_id": selected.ProjectSlug, "query": options.Query, "k": numberValue(k), "max_chunks_per_source": numberValue(max)})
}
func (s *Service) GetSource(ctx context.Context, cwd, project, sourceID string) (map[string]any, error) {
	selected, err := s.Targets.ResolveForCommand(ctx, cwd, project, false)
	if err != nil {
		return nil, err
	}
	return s.Client.Request(ctx, "GET", "/v1/memory/sources/"+sourceID, selected.WorkspaceID, nil)
}

func ValidateInput(file string, stdin bool, label string) error {
	if stdin && file != "" {
		return fmt.Errorf("Provide either --file or --stdin for %s, not both.", label)
	}
	if !stdin && file == "" {
		return fmt.Errorf("Provide --file or --stdin for %s.", label)
	}
	return nil
}
func ParseObject(value, label string) (map[string]any, error) {
	var out any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil, fmt.Errorf("Invalid --%s JSON: %v", label, err)
	}
	object, ok := out.(map[string]any)
	if !ok {
		if label == "metadata" {
			return nil, fmt.Errorf("Invalid --metadata JSON: Metadata must be a JSON object.")
		}
		return nil, fmt.Errorf("Invalid --diff-proof JSON: Diff proof must be a JSON object.")
	}
	return object, nil
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func copyMap(value map[string]any) map[string]any {
	out := map[string]any{}
	for key, item := range value {
		out[key] = item
	}
	return out
}
func valueOr(value any, fallback string) any {
	if value == nil || value == "" {
		return fallback
	}
	return value
}
func numberValue(value float64) any {
	if value == float64(int64(value)) {
		return int64(value)
	}
	return value
}
