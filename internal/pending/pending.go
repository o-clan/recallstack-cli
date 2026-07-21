package pending

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

type Kind string

const (
	KindEvent  Kind = "event"
	KindSource Kind = "source"
)

type LastError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

type Record struct {
	ID            string         `json:"id"`
	Fingerprint   string         `json:"fingerprint"`
	Kind          Kind           `json:"kind"`
	BaseURL       string         `json:"baseUrl"`
	WorkspaceID   string         `json:"workspaceId"`
	ProjectToken  string         `json:"projectToken"`
	Endpoint      string         `json:"endpoint"`
	Body          map[string]any `json:"body"`
	Origin        string         `json:"origin"`
	Cwd           string         `json:"cwd,omitempty"`
	QueuedAt      string         `json:"queuedAt"`
	ReplayCount   int            `json:"replayCount"`
	QueueReason   string         `json:"queueReason"`
	LastError     *LastError     `json:"lastError,omitempty"`
	LastAttemptAt string         `json:"lastAttemptAt,omitempty"`
}

type QueueInput struct {
	Kind         Kind
	BaseURL      string
	WorkspaceID  string
	ProjectToken string
	Body         map[string]any
	Origin       string
	Cwd          string
	QueueReason  string
	LastError    *LastError
}

type QueueResult struct {
	Record    Record
	Duplicate bool
}

type FlushResult struct {
	Attempted     int
	Replayed      int
	Failed        int
	BlockedByAuth bool
	Remaining     int
}

type Sender func(context.Context, Record, config.Config) error

type Store struct {
	Dir   string
	Clock func() time.Time
}

func NewStore(homeDir string) *Store {
	if strings.TrimSpace(homeDir) == "" {
		homeDir, _ = os.UserHomeDir()
	}
	return &Store{Dir: filepath.Join(homeDir, ".recallstack", "pending-writes"), Clock: time.Now}
}

func (s *Store) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func jsISO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

func canonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func stableJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	comparison := collate.New(language.English)
	var writeValue func(reflect.Value) error
	writeValue = func(current reflect.Value) error {
		if !current.IsValid() || ((current.Kind() == reflect.Interface || current.Kind() == reflect.Pointer) && current.IsNil()) {
			buffer.WriteString("null")
			return nil
		}
		for current.Kind() == reflect.Interface || current.Kind() == reflect.Pointer {
			current = current.Elem()
		}
		switch current.Kind() {
		case reflect.Map:
			if current.Type().Key().Kind() != reflect.String {
				return fmt.Errorf("stable JSON requires string map keys")
			}
			keys := current.MapKeys()
			sort.SliceStable(keys, func(i, j int) bool {
				left, right := keys[i].String(), keys[j].String()
				if order := comparison.CompareString(left, right); order != 0 {
					return order < 0
				}
				return left < right
			})
			buffer.WriteByte('{')
			for index, key := range keys {
				if index > 0 {
					buffer.WriteByte(',')
				}
				encodedKey, _ := canonicalJSON(key.String())
				buffer.Write(encodedKey)
				buffer.WriteByte(':')
				if err := writeValue(current.MapIndex(key)); err != nil {
					return err
				}
			}
			buffer.WriteByte('}')
			return nil
		case reflect.Slice, reflect.Array:
			buffer.WriteByte('[')
			for index := 0; index < current.Len(); index++ {
				if index > 0 {
					buffer.WriteByte(',')
				}
				if err := writeValue(current.Index(index)); err != nil {
					return err
				}
			}
			buffer.WriteByte(']')
			return nil
		default:
			raw, err := canonicalJSON(current.Interface())
			if err != nil {
				return err
			}
			buffer.Write(raw)
			return nil
		}
	}
	if err := writeValue(reflect.ValueOf(value)); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func fingerprint(input QueueInput) (string, error) {
	raw, err := stableJSON(map[string]any{
		"kind": string(input.Kind), "baseUrl": input.BaseURL, "workspaceId": input.WorkspaceID,
		"projectToken": input.ProjectToken, "body": input.Body,
	})
	if err != nil {
		return "", err
	}
	sum := sha1.Sum(raw)
	return hex.EncodeToString(sum[:]), nil
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
	return record.ID != "" && record.Fingerprint != "" &&
		(record.Kind == KindEvent || record.Kind == KindSource) &&
		record.BaseURL != "" && record.WorkspaceID != "" && record.ProjectToken != "" &&
		(record.Endpoint == "/v1/memory/events" || record.Endpoint == "/v1/memory/sources") && record.Body != nil
}

func (s *Store) write(record Record) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
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

func (s *Store) Queue(input QueueInput) (QueueResult, error) {
	var result QueueResult
	err := s.withLock(func() error {
		var err error
		result, err = s.queueUnlocked(input)
		return err
	})
	return result, err
}

func (s *Store) queueUnlocked(input QueueInput) (QueueResult, error) {
	fp, err := fingerprint(input)
	if err != nil {
		return QueueResult{}, err
	}
	for _, existing := range s.readAll() {
		if existing.Fingerprint != fp {
			continue
		}
		existing.QueueReason = input.QueueReason
		if input.LastError != nil {
			existing.LastError = input.LastError
		}
		if input.Cwd != "" {
			existing.Cwd = input.Cwd
		}
		if err := s.write(existing); err != nil {
			return QueueResult{}, err
		}
		return QueueResult{Record: existing, Duplicate: true}, nil
	}
	now := s.now()
	idSource := fmt.Sprintf("%s:%d", fp, now.UnixMilli())
	sum := sha1.Sum([]byte(idSource))
	endpoint := "/v1/memory/events"
	if input.Kind == KindSource {
		endpoint = "/v1/memory/sources"
	}
	record := Record{
		ID: hex.EncodeToString(sum[:]), Fingerprint: fp, Kind: input.Kind, BaseURL: input.BaseURL,
		WorkspaceID: input.WorkspaceID, ProjectToken: input.ProjectToken, Endpoint: endpoint,
		Body: input.Body, Origin: input.Origin, Cwd: input.Cwd, QueuedAt: jsISO(now),
		ReplayCount: 0, QueueReason: input.QueueReason, LastError: input.LastError,
	}
	if err := s.write(record); err != nil {
		return QueueResult{}, err
	}
	return QueueResult{Record: record}, nil
}

func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
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

func (s *Store) Flush(ctx context.Context, cfg config.Config, sender Sender) (FlushResult, error) {
	if sender == nil {
		return FlushResult{}, errors.New("pending write sender is required")
	}
	result := FlushResult{}
	err := s.withLock(func() error {
		for _, record := range s.List(cfg.EffectiveBaseURL) {
			result.Attempted++
			err := sender(ctx, record, cfg)
			if err == nil {
				result.Replayed++
				if removeErr := s.Remove(record.ID); removeErr != nil {
					return removeErr
				}
				continue
			}
			record.ReplayCount++
			record.LastAttemptAt = jsISO(s.now())
			record.LastError = &LastError{Message: err.Error(), Code: errorCode(err)}
			if writeErr := s.write(record); writeErr != nil {
				return writeErr
			}
			result.Failed++
			if IsAuthRequired(err) {
				result.BlockedByAuth = true
				break
			}
		}
		result.Remaining = len(s.List(cfg.EffectiveBaseURL))
		return nil
	})
	return result, err
}

var authRequiredPattern = regexp.MustCompile(`(?i)not authenticated|authentication expired|http 401|invalid refresh token|unauthorized`)

func IsAuthRequired(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *api.HTTPError
	return (errors.As(err, &httpErr) && httpErr.Status == 401) || authRequiredPattern.MatchString(err.Error())
}

func AuthQueueReason(err error) string {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == 401 {
		return "HTTP_401"
	}
	message := ""
	if err != nil {
		message = strings.ToLower(err.Error())
	}
	if strings.Contains(message, "authentication expired") {
		return "AUTHENTICATION_EXPIRED"
	}
	if strings.Contains(message, "not authenticated") {
		return "NOT_AUTHENTICATED"
	}
	return "AUTH_REQUIRED"
}

func errorCode(err error) string {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Code
	}
	type coder interface{ Code() string }
	var coded coder
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}
