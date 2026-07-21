package pending

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
)

func TestQueueUsesNodeCompatibleFingerprintAndDeduplicates(t *testing.T) {
	store := &Store{Dir: t.TempDir(), Clock: func() time.Time { return time.UnixMilli(1720000000123) }}
	input := QueueInput{
		Kind: KindEvent, BaseURL: config.DefaultBaseURL, WorkspaceID: "w", ProjectToken: "p", Origin: "cli",
		Body: map[string]any{"project_id": "p", "content": "x"}, QueueReason: "NOT_AUTHENTICATED", Cwd: "/repo",
	}
	first, err := store.Queue(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Record.Fingerprint != "1c7775b58bb7966db7091fece79b138c93877920" {
		t.Fatalf("fingerprint drifted: %s", first.Record.Fingerprint)
	}
	input.QueueReason = "HTTP_401"
	second, err := store.Queue(input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.Record.ID != first.Record.ID || len(store.List("")) != 1 {
		t.Fatalf("not deduplicated: %+v", second)
	}
	if second.Record.QueueReason != "HTTP_401" {
		t.Fatalf("reason not updated: %+v", second.Record)
	}
}

func TestFingerprintUsesNodeLocaleOrderingRecursively(t *testing.T) {
	fingerprint, err := fingerprint(QueueInput{
		Kind: KindEvent, BaseURL: "u", WorkspaceID: "w", ProjectToken: "p",
		Body: map[string]any{"A": 1, "a": 2, "a-b": 3, "a_b": 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != "0c5ae098358f7024645b74bbd89b54fe62c1df89" {
		t.Fatalf("fingerprint differs from Node localeCompare ordering: %s", fingerprint)
	}
}

func TestFlushReplaysInOrderAndStopsOnAuthFailure(t *testing.T) {
	current := time.Unix(100, 0)
	store := &Store{Dir: t.TempDir(), Clock: func() time.Time { current = current.Add(time.Second); return current }}
	for _, content := range []string{"one", "two", "three"} {
		_, err := store.Queue(QueueInput{Kind: KindEvent, BaseURL: "https://one", WorkspaceID: "w", ProjectToken: "p", Body: map[string]any{"content": content}, Origin: "cli", QueueReason: "AUTH_REQUIRED"})
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := []string{}
	result, err := store.Flush(context.Background(), config.Config{EffectiveBaseURL: "https://one"}, func(_ context.Context, record Record, _ config.Config) error {
		content := record.Body["content"].(string)
		seen = append(seen, content)
		if content == "two" {
			return &api.HTTPError{Status: 401, Code: "UNAUTHORIZED", Payload: map[string]any{}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempted != 2 || result.Replayed != 1 || result.Failed != 1 || !result.BlockedByAuth || result.Remaining != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(seen) != 2 || seen[0] != "one" || seen[1] != "two" {
		t.Fatalf("wrong order: %#v", seen)
	}
	if !IsAuthRequired(errors.New("Not authenticated. Run `recallstack login`.")) || AuthQueueReason(errors.New("Authentication expired")) != "AUTHENTICATION_EXPIRED" {
		t.Fatal("auth classification drifted")
	}
}

func TestConcurrentQueueAndFlushAreSingleDelivery(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	input := QueueInput{Kind: KindEvent, BaseURL: "https://one", WorkspaceID: "w", ProjectToken: "p", Body: map[string]any{"content": "same"}, Origin: "hook", QueueReason: "AUTH_REQUIRED"}
	var writers sync.WaitGroup
	for index := 0; index < 12; index++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if _, err := store.Queue(input); err != nil {
				t.Error(err)
			}
		}()
	}
	writers.Wait()
	if records := store.List(""); len(records) != 1 {
		t.Fatalf("concurrent queue created %d records", len(records))
	}
	var deliveries atomic.Int32
	sender := func(context.Context, Record, config.Config) error {
		deliveries.Add(1)
		time.Sleep(20 * time.Millisecond)
		return nil
	}
	var flushers sync.WaitGroup
	for index := 0; index < 2; index++ {
		flushers.Add(1)
		go func() {
			defer flushers.Done()
			if _, err := store.Flush(context.Background(), config.Config{EffectiveBaseURL: "https://one"}, sender); err != nil {
				t.Error(err)
			}
		}()
	}
	flushers.Wait()
	if deliveries.Load() != 1 {
		t.Fatalf("record delivered %d times", deliveries.Load())
	}
}
