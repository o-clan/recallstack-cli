package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/o-clan/recallstack-cli/internal/proof"
	"github.com/o-clan/recallstack-cli/internal/target"
)

type fakeTargets struct {
	value    target.Resolved
	writable bool
}

func (f fakeTargets) ResolveForCommand(_ context.Context, _, _ string, writable bool) (target.Resolved, error) {
	f.writable = writable
	return f.value, nil
}

type fakeClient struct {
	method, path, workspace string
	body                    any
	response                map[string]any
}

func (f *fakeClient) Request(_ context.Context, method, path, workspace string, body any) (map[string]any, error) {
	f.method, f.path, f.workspace, f.body = method, path, workspace, body
	return f.response, nil
}

type fakeProof struct{ close proof.CloseResult }

func (f fakeProof) Start(string, string) error             { return nil }
func (f fakeProof) Close(string, string) proof.CloseResult { return f.close }

func TestIngestBuildsRequestAndFooter(t *testing.T) {
	client := &fakeClient{response: map[string]any{"validation": map[string]any{"applied": true, "status": "verified", "confidence_after": 0.9, "proof_fingerprint": "abc"}}}
	service := Service{Client: client, Targets: fakeTargets{value: target.Resolved{WorkspaceID: "ws", ProjectSlug: "proj", Mode: target.ModeProject}}, Proof: fakeProof{}, Now: func() time.Time { return time.UnixMilli(123) }}
	out, err := service.Ingest(context.Background(), IngestOptions{CWD: ".", Content: "hello", Metadata: map[string]any{"x": true}})
	if err != nil {
		t.Fatal(err)
	}
	body := client.body.(map[string]any)
	if body["project_id"] != "proj" || body["idempotency_key"] != "cli-123" {
		t.Fatalf("unexpected body: %#v", body)
	}
	if !strings.Contains(out["memory_footer"].(string), "status=verified") {
		t.Fatalf("unexpected footer: %#v", out)
	}
}
func TestQueryValidationStartsAfterTarget(t *testing.T) {
	service := Service{Targets: fakeTargets{value: target.Resolved{}}, Proof: fakeProof{}}
	_, err := service.Query(context.Background(), QueryOptions{CWD: ".", Verbose: true})
	if err == nil || err.Error() != "--verbose requires --json." {
		t.Fatalf("unexpected error: %v", err)
	}
}
func TestFormatRetrieve(t *testing.T) {
	response := map[string]any{"evidence": map[string]any{"primary": []any{map[string]any{"id": "m1", "content": "First sentence. More detail.", "confidence": 0.91, "createdAt": "2024-01-01T00:00:00Z", "project": map[string]any{"slug": "api"}}}}}
	got := FormatRetrieve(response, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"STRATEGY", "0.91  [m1] First sentence", "More detail.", "api · 1 day ago"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}
func TestParseObjectRejectsArray(t *testing.T) {
	if _, err := ParseObject(`[]`, "metadata"); err == nil || !strings.Contains(err.Error(), "Metadata must be a JSON object") {
		t.Fatalf("unexpected error: %v", err)
	}
}
