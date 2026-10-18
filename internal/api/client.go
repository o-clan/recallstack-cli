package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type HTTPError struct {
	Status  int
	Payload any
	Code    string
}

func (e *HTTPError) Error() string {
	raw, _ := json.Marshal(e.Payload)
	return fmt.Sprintf("HTTP %d: %s", e.Status, raw)
}

type RequestOptions struct {
	Method      string
	Body        any
	Token       string
	WorkspaceID string
	Headers     map[string]string
	// ResponseHeader, when set, receives the response headers (ETag, Idempotent-Replay).
	ResponseHeader *http.Header
}

// TransportError is a request that never got an HTTP response (offline, DNS,
// refused, timed out). Callers treat it as retryable.
type TransportError struct {
	Message string
	Err     error
}

func (e *TransportError) Error() string { return e.Message }
func (e *TransportError) Unwrap() error { return e.Err }

// Retryable reports whether err should be queued and replayed later: transport
// failures, 429 and 5xx. 401 is handled separately (auth_required).
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var transport *TransportError
	if errors.As(err, &transport) {
		return true
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == http.StatusTooManyRequests || httpErr.Status >= 500
	}
	return false
}

// Status returns the HTTP status of err, or 0.
func Status(err error) int {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status
	}
	return 0
}

// Code returns the error envelope code of err, or "".
func Code(err error) string {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Code
	}
	return ""
}

// Details returns error.details from the envelope.
func Details(err error) map[string]any {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return nil
	}
	root, _ := httpErr.Payload.(map[string]any)
	nested, _ := root["error"].(map[string]any)
	details, _ := nested["details"].(map[string]any)
	return details
}

// Message returns error.message from the envelope.
func Message(err error) string {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return ""
	}
	root, _ := httpErr.Payload.(map[string]any)
	nested, _ := root["error"].(map[string]any)
	message, _ := nested["message"].(string)
	return message
}

type Client struct {
	BaseURL        string
	ClientID       string
	HTTPClient     *http.Client
	DefaultHeaders map[string]string
}

func NewClient(baseURL, clientID string) *Client {
	return &Client{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), ClientID: clientID, HTTPClient: http.DefaultClient}
}

func (c *Client) DoJSON(ctx context.Context, path string, options RequestOptions, out any) error {
	method := options.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if options.Body != nil {
		raw, err := json.Marshal(options.Body)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	target := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	if c.ClientID != "" {
		req.Header.Set("user-agent", c.ClientID)
		req.Header.Set("x-recallstack-client", c.ClientID)
	}
	if options.Token != "" {
		req.Header.Set("authorization", "Bearer "+options.Token)
	}
	if options.WorkspaceID != "" {
		req.Header.Set("x-workspace-id", options.WorkspaceID)
	}
	for key, value := range c.DefaultHeaders {
		req.Header.Set(key, value)
	}
	for key, value := range options.Headers {
		req.Header.Set(key, value)
	}
	if req.Header.Get("traceparent") == "" {
		traceID, spanID, ok := newTraceContext()
		if ok {
			req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
			req.Header.Set("x-trace-id", traceID[:8]+"-"+traceID[8:12]+"-"+traceID[12:16]+"-"+traceID[16:20]+"-"+traceID[20:])
		}
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return wrapTransportError(target, err)
	}
	defer response.Body.Close()
	if options.ResponseHeader != nil {
		*options.ResponseHeader = response.Header.Clone()
	}
	if response.StatusCode == http.StatusNotModified {
		return &HTTPError{Status: http.StatusNotModified, Payload: map[string]any{}}
	}
	raw, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return &TransportError{Message: readErr.Error(), Err: readErr}
	}
	var payload any = map[string]any{}
	validJSON := false
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			payload = map[string]any{}
		} else {
			validJSON = true
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := errorCode(payload)
		if response.StatusCode == http.StatusForbidden && code == "LEGAL_CONSENT_REQUIRED" {
			consentURL, _ := url.Parse(c.BaseURL)
			consentURL.Path = "/consent"
			consentURL.RawQuery = "next=%2Fdashboard"
			return fmt.Errorf("Legal consent required. Open %s, accept Terms and Privacy, then retry.", consentURL.String())
		}
		return &HTTPError{Status: response.StatusCode, Payload: payload, Code: code}
	}
	if out == nil {
		return nil
	}
	if rawMessage, ok := out.(*json.RawMessage); ok {
		if validJSON {
			*rawMessage = append((*rawMessage)[:0], raw...)
		} else {
			*rawMessage = append((*rawMessage)[:0], []byte("{}")...)
		}
		return nil
	}
	if !validJSON {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, out)
}

func newTraceContext() (string, string, bool) {
	traceBytes := make([]byte, 16)
	spanBytes := make([]byte, 8)
	if _, err := rand.Read(traceBytes); err != nil {
		return "", "", false
	}
	if _, err := rand.Read(spanBytes); err != nil {
		return "", "", false
	}
	return hex.EncodeToString(traceBytes), hex.EncodeToString(spanBytes), true
}

// NewTraceContext creates W3C propagation headers and returns their raw IDs so
// the caller can relay a matching command span after execution.
func NewTraceContext() (headers map[string]string, traceID, spanID string, ok bool) {
	traceID, spanID, ok = newTraceContext()
	if !ok {
		return nil, "", "", false
	}
	uuid := traceID[:8] + "-" + traceID[8:12] + "-" + traceID[12:16] + "-" + traceID[16:20] + "-" + traceID[20:]
	return map[string]string{
		"traceparent": "00-" + traceID + "-" + spanID + "-01",
		"x-trace-id":  uuid,
	}, traceID, spanID, true
}

func errorCode(payload any) string {
	root, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	if nested, ok := root["error"].(map[string]any); ok {
		if code, ok := nested["code"].(string); ok {
			return code
		}
	}
	if code, ok := root["code"].(string); ok {
		return code
	}
	return ""
}

func wrapTransportError(target string, err error) error {
	message := fmt.Sprintf("Request to %s failed: %s.", target, err.Error())
	parsed, parseErr := url.Parse(target)
	if parseErr != nil || parsed.Scheme != "https" || !isLocalhost(parsed.Hostname()) {
		return &TransportError{Message: message, Err: err}
	}
	text := strings.ToLower(err.Error())
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		text += " " + strings.ToLower(opErr.Error())
	}
	if strings.Contains(text, "server gave http response to https client") || strings.Contains(text, "first record does not look like a tls handshake") {
		return &TransportError{Message: fmt.Sprintf("%s Local Recallstack dev listens on http by default. Try `recallstack config base-url http://%s` or enable HTTPS for your local API", message, parsed.Host), Err: err}
	}
	if strings.Contains(text, "certificate") || strings.Contains(text, "x509") {
		return &TransportError{Message: fmt.Sprintf("%s Local HTTPS certificate validation failed. Trust your local certificate, use a hostname covered by the cert, or switch to `http://%s` for the default dev stack", message, parsed.Host), Err: err}
	}
	return &TransportError{Message: message, Err: err}
}

func isLocalhost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
