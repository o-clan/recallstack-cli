package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestDoJSONSendsCompatibilityHeadersAndDecodesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("X-Workspace-Id") != "ws" || r.Header.Get("X-Recallstack-Client") != "recallstack-cli/1.1.0" {
			t.Errorf("unexpected headers: %#v", r.Header)
		}
		if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(r.Header.Get("traceparent")) {
			t.Errorf("invalid traceparent: %q", r.Header.Get("traceparent"))
		}
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(r.Header.Get("x-trace-id")) {
			t.Errorf("invalid x-trace-id: %q", r.Header.Get("x-trace-id"))
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "recallstack-cli/1.1.0")
	client.DefaultHeaders = map[string]string{
		"traceparent": "00-11111111111111111111111111111111-2222222222222222-01",
		"x-trace-id":  "11111111-1111-1111-1111-111111111111",
	}
	err := client.DoJSON(context.Background(), "/v1/test", RequestOptions{Token: "token", WorkspaceID: "ws"}, &map[string]any{})
	httpErr, ok := err.(*HTTPError)
	if !ok || httpErr.Status != 401 || httpErr.Code != "UNAUTHORIZED" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestDoJSONPreservesRawResponseAndLegalConsentMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/consent-needed" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"LEGAL_CONSENT_REQUIRED"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "test")
	var raw json.RawMessage
	if err := client.DoJSON(context.Background(), "/ok", RequestOptions{}, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("unexpected raw response: %s", raw)
	}
	err := client.DoJSON(context.Background(), "/consent-needed", RequestOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "/consent?next=%2Fdashboard") {
		t.Fatalf("unexpected consent error: %v", err)
	}
}
