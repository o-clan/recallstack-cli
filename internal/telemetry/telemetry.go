package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/auth"
	"github.com/o-clan/recallstack-cli/internal/config"
)

type Event struct {
	Name       string
	Operation  string
	Outcome    string
	ProjectID  string
	DurationMS int64
	Attributes map[string]any
}

type Trace struct {
	TraceID   string
	SpanID    string
	Name      string
	ClientID  string
	StartedAt time.Time
	EndedAt   time.Time
	Outcome   string
}

// Emit sends CLI telemetry on a best-effort basis. Telemetry must never change
// the outcome of the command that produced it.
func Emit(ctx context.Context, manager *auth.Manager, cfg *config.Config, event Event) {
	if manager == nil {
		return
	}
	outcome := event.Outcome
	if outcome == "" {
		outcome = "success"
	}
	body := map[string]any{
		"source":     "cli",
		"event_name": event.Name,
		"operation":  event.Operation,
		"outcome":    outcome,
	}
	if event.ProjectID != "" {
		body["project_id"] = event.ProjectID
	}
	if event.DurationMS > 0 {
		body["duration_ms"] = event.DurationMS
	}
	if len(event.Attributes) > 0 {
		body["attributes"] = event.Attributes
	}
	var ignored any
	_ = manager.DoJSON(ctx, cfg, "/v1/telemetry/events", api.RequestOptions{
		Method: http.MethodPost,
		Body:   body,
	}, &ignored)
}

// EmitTrace relays the command span that owns the W3C context propagated by
// the API client. Like event telemetry, relay failure never affects commands.
func EmitTrace(ctx context.Context, manager *auth.Manager, cfg *config.Config, span Trace) {
	if manager == nil || span.TraceID == "" || span.SpanID == "" {
		return
	}
	status := "ok"
	if span.Outcome == "error" {
		status = "error"
	}
	body := map[string]any{
		"source":              "cli",
		"service_name":        "recallstack-cli",
		"client_name":         span.ClientID,
		"resource_attributes": map[string]any{"surface": "cli"},
		"spans": []any{map[string]any{
			"trace_id":             span.TraceID,
			"span_id":              span.SpanID,
			"parent_span_id":       nil,
			"name":                 span.Name,
			"start_time_unix_nano": strconv.FormatInt(span.StartedAt.UnixNano(), 10),
			"end_time_unix_nano":   strconv.FormatInt(span.EndedAt.UnixNano(), 10),
			"status":               status,
			"attributes":           map[string]any{"surface": "cli", "client.name": span.ClientID},
		}},
	}
	var ignored any
	_ = manager.DoJSON(ctx, cfg, "/v1/telemetry/traces", api.RequestOptions{Method: http.MethodPost, Body: body}, &ignored)
}
