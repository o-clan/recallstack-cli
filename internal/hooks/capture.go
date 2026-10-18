package hooks

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/pending"
	"github.com/o-clan/recallstack-cli/internal/project"
	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/secrets"
	"github.com/o-clan/recallstack-cli/internal/state"
)

// job is one end-of-turn capture, handed from the hook to a detached worker.
type job struct {
	Agent          string         `json:"agent"`
	SessionID      string         `json:"session_id"`
	CWD            string         `json:"cwd"`
	TranscriptPath string         `json:"transcript_path,omitempty"`
	TurnID         string         `json:"turn_id,omitempty"`
	Prompt         string         `json:"prompt,omitempty"`
	Response       string         `json:"response,omitempty"`
	GenerationID   string         `json:"generation_id,omitempty"`
	Payload        map[string]any `json:"payload,omitempty"`
	QueuedAt       string         `json:"queued_at"`
}

// stop queues the capture and returns at once.
func (h *hookRun) stop() string {
	sessionID := h.sessionID()
	if sessionID == "" {
		return "no_session_id"
	}
	value := job{
		Agent: h.agent, SessionID: sessionID, CWD: h.cwd,
		TranscriptPath: firstNonEmpty(stringValue(h.payload["transcript_path"]), stringValue(h.payload["transcriptPath"])),
		TurnID:         stringValue(h.payload["turn_id"]),
		Payload:        h.payload,
		QueuedAt:       time.Now().UTC().Format(time.RFC3339Nano),
	}
	if h.agent == "cursor" {
		pair := loadCapture(h.agent, sessionID)
		value.Prompt, value.Response = pair.LastPrompt, pair.LastResponse
		value.GenerationID = firstNonEmpty(stringValue(h.payload["generation_id"]), pair.LastResponseGeneration, pair.LastPromptGeneration)
		if value.Prompt == "" && value.Response == "" {
			spawnRefresh(h.agent, h.version, sessionID)
			return "nothing_to_capture"
		}
	}
	if h.agent == "copilot" {
		value.Prompt = loadCapture(h.agent, sessionID).LastPrompt
	}
	path := filepath.Join(state.JobsDir(), h.agent+"-"+sha1Hex(sessionID, value.QueuedAt)[:16]+".json")
	if err := state.WriteJSON(path, value); err != nil {
		return "error: " + err.Error()
	}
	if _, err := spawnSelf(hiddenWorkerCommand, h.agent, path); err != nil {
		return "spawn_failed"
	}
	return "queued"
}

// captureState is per session: what's already been captured.
type captureState struct {
	LastTurnKey            string `json:"last_turn_key,omitempty"`
	LastStep               int    `json:"last_step"`
	LastPrompt             string `json:"last_prompt,omitempty"`
	LastPromptGeneration   string `json:"last_prompt_generation_id,omitempty"`
	LastResponse           string `json:"last_response,omitempty"`
	LastResponseGeneration string `json:"last_response_generation_id,omitempty"`
	LastCaptureAt          string `json:"last_capture_at,omitempty"`
	LastSentAt             string `json:"last_sent_at,omitempty"`
}

func capturePath(agent, sessionID string) string {
	return filepath.Join(state.CaptureDir(), agent+"-"+state.SafeName(sessionID)+".json")
}

func loadCapture(agent, sessionID string) captureState {
	value := captureState{LastStep: -1}
	state.ReadJSON(capturePath(agent, sessionID), &value)
	return value
}

func saveCapture(agent, sessionID string, value captureState) {
	_ = state.WriteJSON(capturePath(agent, sessionID), value)
}

// cursorPair keeps Cursor's prompt and response so stop can pair them.
func (h *hookRun) cursorPair() string {
	sessionID := h.sessionID()
	if sessionID == "" {
		return "no_session_id"
	}
	value := loadCapture(h.agent, sessionID)
	generation := stringValue(h.payload["generation_id"])
	if h.moment == momentCursorPrompt {
		if prompt := firstString(h.payload, "prompt", "text"); prompt != "" {
			value.LastPrompt, value.LastPromptGeneration = prompt, generation
		}
		if _, known := state.LoadSession(h.agent, sessionID); !known {
			folder := project.Detect(h.cwd)
			session := state.Session{Agent: h.agent, SessionID: sessionID, CWD: folder.CWD, CWDKey: folder.Key, RepoRoot: folder.RepoRoot, StartedAt: time.Now().UTC().Format(time.RFC3339)}
			if entry, ok := project.Lookup(folder.Key); ok {
				session.ProjectID, session.ProjectName = entry.ProjectID, entry.Name
			}
			_ = state.SaveSession(session)
		}
	} else {
		if response := firstString(h.payload, "text", "response", "agent_message", "message"); response != "" {
			value.LastResponse, value.LastResponseGeneration = response, generation
		}
	}
	saveCapture(h.agent, sessionID, value)
	return "ok"
}

// Worker processes one queued capture job.
func Worker(ctx context.Context, agentArg, jobPath string, options Options) error {
	agent := NormalizeAgent(agentArg)
	var value job
	if !state.ReadJSON(jobPath, &value) || value.SessionID == "" {
		state.Remove(jobPath)
		return nil
	}
	defer state.Remove(jobPath)
	outcome := captureJob(ctx, agent, value, options.Version)
	state.AppendLog("capture.log", map[string]any{"agent": agent, "session_id": value.SessionID, "outcome": outcome})
	if agent == "cursor" || agent == "antigravity" || agent == "copilot" {
		refreshBrief(ctx, agent, options.Version, value.SessionID)
	}
	return nil
}

func captureJob(ctx context.Context, agent string, value job, version string) string {
	capture := loadCapture(agent, value.SessionID)
	turns := []turn{}
	switch agent {
	case "claude":
		if found, ok := latestClaudeTurn(value.TranscriptPath, value.Payload); ok {
			turns = append(turns, found)
		}
	case "codex":
		for attempt := 0; attempt < 40; attempt++ {
			found, ok := latestCodexTurn(value.TranscriptPath)
			if ok && (value.TurnID == "" || found.TurnID == value.TurnID) {
				turns = []turn{found}
				break
			}
			if ok {
				turns = []turn{found}
			}
			time.Sleep(250 * time.Millisecond)
		}
	case "cursor":
		if value.Prompt != "" {
			key := firstNonEmpty(value.GenerationID, sha1Hex(value.Prompt, value.Response))
			turns = append(turns, turn{DedupeKey: key, TurnID: key, UserMessages: []string{value.Prompt}, AssistantMessages: nonEmptySlice(value.Response), LastAssistant: value.Response})
		}
	case "copilot":
		if found, ok := findCopilotTurn(value.Payload, repoRootOf(value.CWD), value.Prompt); ok {
			turns = append(turns, found)
		}
	case "antigravity":
		turns = antigravityTurns(value.TranscriptPath, stringValue(value.Payload["conversationId"]), capture.LastStep)
	}
	if len(turns) == 0 {
		return "no_turn"
	}
	session, _ := state.LoadSession(agent, value.SessionID)
	folder := project.Detect(firstNonEmpty(value.CWD, session.CWD))
	svc := remote.New(version)
	projectID := session.ProjectID
	if projectID == "" {
		if entry, _, err := project.Resolve(ctx, svc, folder); err == nil {
			projectID = entry.ProjectID
		}
	}
	brief, _, _ := state.LoadBriefCache(projectID)
	person := state.PersonName()
	sent, skipped := 0, 0
	for _, current := range turns {
		key := firstNonEmpty(current.TurnID, current.DedupeKey)
		if agent != "antigravity" && key != "" && key == capture.LastTurnKey {
			skipped++
			continue
		}
		step := current.AssistantStepIndex
		markDone := func() {
			capture.LastTurnKey = key
			if step > capture.LastStep {
				capture.LastStep = step
			}
			capture.LastCaptureAt = time.Now().UTC().Format(time.RFC3339)
			saveCapture(agent, value.SessionID, capture)
		}
		userText := compact(last(current.UserMessages))
		if userText == "" || pleasantryPattern.MatchString(userText) {
			markDone()
			skipped++
			continue
		}
		result, runner, err := extract(ctx, agent, person, brief, current)
		if err != nil {
			state.AppendLog("capture.log", map[string]any{"agent": agent, "session_id": value.SessionID, "outcome": "extract_failed", "runner": runner, "error": truncate(err.Error(), 600)})
			markDone()
			continue
		}
		candidates, secretCount := normalizeCandidates(result.Candidates, current.UserMessages)
		var actedAgainst any
		if result.ActedAgainst != nil && strings.TrimSpace(result.ActedAgainst.Ref) != "" {
			actedAgainst = map[string]any{"ref": strings.TrimSpace(result.ActedAgainst.Ref), "what_happened": truncate(stripSecrets(result.ActedAgainst.WhatHappened), 300)}
		}
		markDone()
		if len(candidates) == 0 && actedAgainst == nil {
			skipped++
			continue
		}
		var projectValue any
		if projectID != "" {
			projectValue = projectID
		}
		body := map[string]any{
			"project_id": projectValue, "agent": agent, "session_id": value.SessionID, "turn_key": key,
			"origin_key": folder.Key, "candidates": candidates, "acted_against": actedAgainst,
		}
		var ignored any
		queued, err := svc.Send(ctx, remote.Write{
			Path: "/v1/records/candidates", Body: toMap(body), Label: "candidates",
			IdempotencyKey: agent + "-" + sha1Hex(value.SessionID+":"+key), Origin: agent + "_stop",
		}, &ignored)
		state.AppendLog("capture.log", map[string]any{"agent": agent, "session_id": value.SessionID, "turn_key": key, "candidates": len(candidates), "secrets_stripped": secretCount, "queued": queued, "error": errorText(err)})
		if err == nil {
			sent++
			capture.LastSentAt = time.Now().UTC().Format(time.RFC3339)
			saveCapture(agent, value.SessionID, capture)
		}
		if queued {
			_, _ = spawnSelf(hiddenRetryCommand, agent, "queue")
		}
	}
	return fmt.Sprintf("sent=%d skipped=%d", sent, skipped)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return truncate(err.Error(), 400)
}

func stripSecrets(value string) string { return secrets.StripString(value) }

func repoRootOf(cwd string) string {
	folder := project.Detect(cwd)
	if folder.RepoRoot != "" {
		return folder.RepoRoot
	}
	return cwd
}

// Retry keeps replaying the pending queue in the background, with backoff,
// for up to the retry window (24 hours). Only one runs at a time.
func Retry(ctx context.Context, _ string, _ string, options Options) error {
	lock := filepath.Join(state.JobsDir(), "retry.pid")
	if raw, err := os.ReadFile(lock); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(raw))); pid > 0 && processAlive(pid) {
			return nil
		}
	}
	_ = state.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())))
	defer state.Remove(lock)
	svc := remote.New(options.Version)
	base := durationEnv("RECALLSTACK_INGEST_RETRY_BASE_MS", 30*time.Second)
	maxDelay := durationEnv("RECALLSTACK_INGEST_RETRY_MAX_MS", 15*time.Minute)
	deadline := time.Now().Add(pending.Window())
	delay := base
	for time.Now().Before(deadline) {
		time.Sleep(delay)
		flushCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		result, err := svc.Flush(flushCtx)
		cancel()
		state.AppendLog("retry.log", map[string]any{"replayed": result.Replayed, "remaining": result.Remaining, "blocked_by_auth": result.BlockedByAuth, "error": errorText(err)})
		if err == nil && (result.Remaining == 0 || result.BlockedByAuth) {
			return nil
		}
		if delay *= 2; delay > maxDelay {
			delay = maxDelay
		}
	}
	return nil
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}

// Refresh rewrites the cached brief (and the agent's brief file, for agents
// without a start hook) in the background.
func Refresh(ctx context.Context, agentArg, sessionID string, options Options) error {
	refreshBrief(ctx, NormalizeAgent(agentArg), options.Version, sessionID)
	return nil
}

type briefResponse struct {
	V    int    `json:"v"`
	Text string `json:"text"`
}

// FetchBrief asks for the brief the next session of agent would get.
func FetchBrief(ctx context.Context, svc *remote.Service, projectID, agent string, all bool) (briefResponse, error) {
	query := url.Values{}
	if projectID != "" {
		query.Set("project_id", projectID)
	}
	query.Set("agent", agent)
	if all {
		query.Set("all", "1")
	}
	var response briefResponse
	err := svc.Do(ctx, remote.Request{Method: http.MethodGet, Path: "/v1/brief?" + query.Encode()}, &response)
	return response, err
}

func refreshBrief(ctx context.Context, agent, version, sessionID string) {
	session, known := state.LoadSession(agent, sessionID)
	if !known {
		return
	}
	svc := remote.New(version)
	if !svc.SignedIn() {
		return
	}
	if session.ProjectID == "" {
		if entry, _, err := project.Resolve(ctx, svc, project.Detect(session.CWD)); err == nil {
			session.ProjectID, session.ProjectName = entry.ProjectID, entry.Name
			_ = state.SaveSession(session)
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := FetchBrief(requestCtx, svc, session.ProjectID, agent, false)
	if err != nil || strings.TrimSpace(response.Text) == "" {
		return
	}
	_ = state.SaveBriefCache(session.ProjectID, response.Text)
	switch agent {
	case "cursor":
		_ = writeCursorBrief(firstNonEmpty(session.RepoRoot, session.CWD), response.Text)
	case "copilot":
		_ = writeCopilotBrief(response.Text)
	case "antigravity":
		_ = writeAntigravityBrief(response.Text)
	}
}

// SpawnRetry starts the background queue replay (it exits at once if one runs).
func SpawnRetry() { _, _ = spawnSelf(hiddenRetryCommand, "cli", "queue") }

func spawnRefresh(agent, version, sessionID string) {
	_, _ = spawnSelf(hiddenRefreshCommand, agent, sessionID)
}

// spawnSelf starts this executable detached with the hidden command.
func spawnSelf(command, agent, arg string) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cwd, _ := os.Getwd()
	return startDetached(executable, []string{command, agent, arg}, cwd, os.Environ())
}

func toMap(value map[string]any) map[string]any { return value }
