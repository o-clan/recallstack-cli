// Package pending is the queue of writes that couldn't be sent yet: offline,
// rate limited, server errors, or waiting on sign-in. Each write is replayed
// with its Idempotency-Key, so the server applies it once.
package pending

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
)

type LastError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Status  int    `json:"status,omitempty"`
}

type Record struct {
	ID             string         `json:"id"`
	Fingerprint    string         `json:"fingerprint"`
	BaseURL        string         `json:"baseUrl"`
	Method         string         `json:"method"`
	Endpoint       string         `json:"endpoint"`
	Body           map[string]any `json:"body"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
	Label          string         `json:"label,omitempty"`
	ChangeID       string         `json:"changeId,omitempty"`
	Origin         string         `json:"origin"`
	Cwd            string         `json:"cwd,omitempty"`
	QueuedAt       string         `json:"queuedAt"`
	ReplayCount    int            `json:"replayCount"`
	QueueReason    string         `json:"queueReason"`
	LastError      *LastError     `json:"lastError,omitempty"`
	LastAttemptAt  string         `json:"lastAttemptAt,omitempty"`
}

type QueueInput struct {
	BaseURL        string
	Method         string
	Endpoint       string
	Body           map[string]any
	IdempotencyKey string
	Label          string
	ChangeID       string
	Origin         string
	Cwd            string
	QueueReason    string
	LastError      *LastError
}

type QueueResult struct {
	Record    Record
	Duplicate bool
}

type FlushResult struct {
	Attempted     int
	Replayed      int
	Failed        int
	Dropped       int
	Expired       int
	BlockedByAuth bool
	Remaining     int
}

type Sender func(context.Context, Record, config.Config) error

type Store struct {
	Dir   string
	Clock func() time.Time
}

// NewStore opens the queue in dir (state.PendingDir() for the real one).
func NewStore(dir string) *Store { return &Store{Dir: dir, Clock: time.Now} }

// Window is how long a retryable write is kept (24 hours by default).
func Window() time.Duration {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("RECALLSTACK_INGEST_RETRY_WINDOW_MS")), 10, 64)
	if err != nil || value <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(value) * time.Millisecond
}

func (s *Store) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

const isoLayout = "2006-01-02T15:04:05.000Z"

func jsISO(value time.Time) string { return value.UTC().Format(isoLayout) }

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

func fingerprint(input QueueInput) string {
	seed := input.BaseURL + "\n" + input.IdempotencyKey
	if input.IdempotencyKey == "" {
		body, _ := json.Marshal(input.Body)
		seed = input.BaseURL + "\n" + input.Method + "\n" + input.Endpoint + "\n" + string(body)
	}
	sum := sha1.Sum([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func (s *Store) readAll() []Record {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.Dir, entry.Name()))
		if err != nil {
			continue
		}
		var record Record
		if json.Unmarshal(raw, &record) != nil || !valid(record) {
			continue
		}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].QueuedAt < records[j].QueuedAt })
	return records
}

func valid(record Record) bool {
	return record.ID != "" && record.Fingerprint != "" && record.BaseURL != "" &&
		strings.HasPrefix(record.Endpoint, "/v1/") && record.Body != nil &&
		(record.Method == "POST" || record.Method == "PATCH" || record.Method == "PUT")
}

func (s *Store) write(record Record) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(s.Dir, ".pending-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(record.ID))
}

// Queue adds a write, or refreshes the existing one with the same key.
func (s *Store) Queue(input QueueInput) (QueueResult, error) {
	var result QueueResult
	err := s.withLock(func() error {
		fp := fingerprint(input)
		for _, existing := range s.readAll() {
			if existing.Fingerprint != fp {
				continue
			}
			existing.QueueReason = input.QueueReason
			if input.LastError != nil {
				existing.LastError = input.LastError
			}
			result = QueueResult{Record: existing, Duplicate: true}
			return s.write(existing)
		}
		now := s.now()
		sum := sha1.Sum([]byte(fmt.Sprintf("%s:%d", fp, now.UnixNano())))
		method := input.Method
		if method == "" {
			method = "POST"
		}
		record := Record{
			ID: hex.EncodeToString(sum[:]), Fingerprint: fp, BaseURL: input.BaseURL, Method: method,
			Endpoint: input.Endpoint, Body: input.Body, IdempotencyKey: input.IdempotencyKey,
			Label: input.Label, ChangeID: input.ChangeID, Origin: input.Origin, Cwd: input.Cwd,
			QueuedAt: jsISO(now), QueueReason: input.QueueReason, LastError: input.LastError,
		}
		result = QueueResult{Record: record}
		return s.write(record)
	})
	return result, err
}

func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(s.Dir, ".queue.lock")
	deadline := time.Now().Add(30 * time.Second)
	for {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = lock.WriteString(time.Now().UTC().Format(time.RFC3339Nano))
			_ = lock.Close()
			defer os.Remove(lockPath)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 5*time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for Recallstack pending-write lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// List returns queued writes for baseURL (all when empty), oldest first.
func (s *Store) List(baseURL string) []Record {
	records := s.readAll()
	if baseURL == "" {
		return records
	}
	filtered := make([]Record, 0, len(records))
	for _, record := range records {
		if record.BaseURL == baseURL {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func (s *Store) Remove(id string) error {
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RemoveByID removes one queued write under the queue lock.
func (s *Store) RemoveByID(id string) (bool, error) {
	removed := false
	err := s.withLock(func() error {
		if _, statErr := os.Stat(s.path(id)); statErr == nil {
			removed = true
		}
		return s.Remove(id)
	})
	return removed, err
}

// FindByChangeID returns the queued write carrying changeID.
func (s *Store) FindByChangeID(changeID string) (Record, bool) {
	if changeID == "" {
		return Record{}, false
	}
	for _, record := range s.readAll() {
		if record.ChangeID == changeID {
			return record, true
		}
	}
	return Record{}, false
}

// Flush replays queued writes for cfg's base URL, oldest first. A write that
// fails for good (a 4xx other than 401 and 429) is dropped; a retryable one
// stays; a 401 stops the flush until the next sign-in. Retryable writes older
// than Window() expire; writes waiting on sign-in don't.
func (s *Store) Flush(ctx context.Context, cfg config.Config, sender Sender) (FlushResult, error) {
	if sender == nil {
		return FlushResult{}, errors.New("pending write sender is required")
	}
	result := FlushResult{}
	err := s.withLock(func() error {
		window := Window()
		for _, record := range s.List(cfg.EffectiveBaseURL) {
			if ctx.Err() != nil {
				break
			}
			queuedAt, _ := time.Parse(isoLayout, record.QueuedAt)
			if record.QueueReason != "auth_required" && !queuedAt.IsZero() && s.now().Sub(queuedAt) > window {
				result.Expired++
				_ = s.Remove(record.ID)
				continue
			}
			result.Attempted++
			err := sender(ctx, record, cfg)
			if err == nil {
				result.Replayed++
				if removeErr := s.Remove(record.ID); removeErr != nil {
					return removeErr
				}
				continue
			}
			auth := IsAuthRequired(err)
			if !auth && !api.Retryable(err) {
				result.Dropped++
				_ = s.Remove(record.ID)
				continue
			}
			if auth {
				record.QueueReason = "auth_required"
			}
			record.ReplayCount++
			record.LastAttemptAt = jsISO(s.now())
			record.LastError = &LastError{Message: err.Error(), Code: api.Code(err), Status: api.Status(err)}
			if writeErr := s.write(record); writeErr != nil {
				return writeErr
			}
			result.Failed++
			if auth {
				result.BlockedByAuth = true
				break
			}
		}
		result.Remaining = len(s.List(cfg.EffectiveBaseURL))
		return nil
	})
	return result, err
}

var authRequiredPattern = regexp.MustCompile(`(?i)not signed in|not authenticated|authentication expired|http 401|invalid refresh token|unauthorized`)

// IsAuthRequired reports a 401, or an auth manager "not signed in" error.
func IsAuthRequired(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == 401
	}
	var transport *api.TransportError
	if errors.As(err, &transport) {
		return false
	}
	return authRequiredPattern.MatchString(err.Error())
}
