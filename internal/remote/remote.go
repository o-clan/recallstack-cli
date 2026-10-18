// Package remote is the one way commands and hooks talk to the Recallstack API.
// Config and the keyring are read lazily, so a hook that never needs the
// network never pays for them.
package remote

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/auth"
	"github.com/o-clan/recallstack-cli/internal/config"
	"github.com/o-clan/recallstack-cli/internal/pending"
	"github.com/o-clan/recallstack-cli/internal/state"
)

type Service struct {
	Version string
	Store   *config.Store
	Auth    *auth.Manager

	once   sync.Once
	cfg    config.Config
	cfgErr error
}

func New(version string) *Service {
	store := config.NewStore()
	clientID := "recallstack-cli/" + version
	manager := auth.NewManager(store, "", clientID)
	manager.Headers = map[string]string{"X-Recallstack-TZ": TimeZone()}
	return &Service{Version: version, Store: store, Auth: manager}
}

// Config loads config (and keyring tokens) once.
func (s *Service) Config() (config.Config, error) {
	s.once.Do(func() { s.cfg, s.cfgErr = s.Store.Load("") })
	return s.cfg, s.cfgErr
}

// Reload forgets the cached config (after sign-in or sign-out).
func (s *Service) Reload() (config.Config, error) {
	s.once = sync.Once{}
	return s.Config()
}

func (s *Service) BaseURL() string {
	cfg, _ := s.Config()
	if cfg.EffectiveBaseURL == "" {
		return config.DefaultBaseURL
	}
	return cfg.EffectiveBaseURL
}

func (s *Service) SignedIn() bool {
	cfg, err := s.Config()
	return err == nil && (cfg.AccessToken != "" || cfg.RefreshToken != "" || cfg.APIKey != "")
}

// Request is one API call.
type Request struct {
	Method         string
	Path           string
	Body           any
	IdempotencyKey string
	Headers        map[string]string
	ResponseHeader *http.Header
}

// Do runs an authenticated request.
func (s *Service) Do(ctx context.Context, request Request, out any) error {
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	headers := map[string]string{}
	for key, value := range request.Headers {
		headers[key] = value
	}
	if request.IdempotencyKey != "" {
		headers["Idempotency-Key"] = request.IdempotencyKey
	}
	return s.Auth.DoJSON(ctx, &cfg, request.Path, api.RequestOptions{
		Method: request.Method, Body: request.Body, Headers: headers, ResponseHeader: request.ResponseHeader,
	}, out)
}

// Write is a change that must reach the server eventually.
type Write struct {
	Method         string
	Path           string
	Body           map[string]any
	IdempotencyKey string
	Label          string
	ChangeID       string
	Origin         string
}

// Send tries a write now. When it can't be delivered yet (offline, 429, 5xx,
// signed out), it goes to the pending queue and Send reports queued=true.
// Other errors (4xx) come back as they are.
func (s *Service) Send(ctx context.Context, write Write, out any) (queued bool, err error) {
	method := write.Method
	if method == "" {
		method = http.MethodPost
	}
	err = s.Do(ctx, Request{Method: method, Path: write.Path, Body: write.Body, IdempotencyKey: write.IdempotencyKey}, out)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, context.DeadlineExceeded) || api.Retryable(err) || pending.IsAuthRequired(err) {
		reason := "retryable"
		if pending.IsAuthRequired(err) {
			reason = "auth_required"
		}
		_, queueErr := s.Queue().Queue(pending.QueueInput{
			BaseURL: s.BaseURL(), Method: method, Endpoint: write.Path, Body: write.Body,
			IdempotencyKey: write.IdempotencyKey, Label: write.Label, ChangeID: write.ChangeID,
			Origin: write.Origin, QueueReason: reason,
			LastError: &pending.LastError{Message: err.Error(), Code: api.Code(err), Status: api.Status(err)},
		})
		if queueErr != nil {
			return false, queueErr
		}
		return true, nil
	}
	return false, err
}

func (s *Service) Queue() *pending.Store { return pending.NewStore(state.PendingDir()) }

// Flush replays the pending queue within ctx.
func (s *Service) Flush(ctx context.Context) (pending.FlushResult, error) {
	cfg, err := s.Config()
	if err != nil {
		return pending.FlushResult{}, err
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" && cfg.APIKey == "" {
		return pending.FlushResult{Remaining: len(s.Queue().List(cfg.EffectiveBaseURL)), BlockedByAuth: true}, nil
	}
	return s.Queue().Flush(ctx, cfg, func(ctx context.Context, record pending.Record, cfg config.Config) error {
		var ignored any
		return s.Do(ctx, Request{Method: record.Method, Path: record.Endpoint, Body: record.Body, IdempotencyKey: record.IdempotencyKey}, &ignored)
	})
}

// FlushQuietly replays the queue in the background of a command, bounded by d.
func (s *Service) FlushQuietly(d time.Duration) pending.FlushResult {
	if len(s.Queue().List("")) == 0 {
		return pending.FlushResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	result, _ := s.Flush(ctx)
	return result
}

// Me fetches the signed-in person and caches it for offline lines.
func (s *Service) Me(ctx context.Context) (state.Me, error) {
	var me state.Me
	if err := s.Do(ctx, Request{Method: http.MethodGet, Path: "/v1/me"}, &me); err != nil {
		return state.LoadMe(), err
	}
	cached := state.LoadMe()
	me.DeviceName = cached.DeviceName
	_ = state.SaveMe(me)
	return me, nil
}

// TimeZone is the IANA zone for X-Recallstack-TZ.
func TimeZone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" && !strings.HasPrefix(tz, ":") && strings.Contains(tz, "/") {
		return tz
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(raw)); tz != "" {
			return tz
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if index := strings.Index(target, "zoneinfo/"); index >= 0 {
			return target[index+len("zoneinfo/"):]
		}
		return filepath.Base(target)
	}
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	return "UTC"
}
