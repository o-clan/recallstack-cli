package proof

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	SnapshotTTL = 24 * time.Hour
	MaxBytes    = 65536
)

type File struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Excerpt   string `json:"excerpt,omitempty"`
}

type Summary struct {
	FilesChanged int `json:"files_changed"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
}
type Repo struct {
	VCS          string `json:"vcs,omitempty"`
	HeadStart    string `json:"head_start,omitempty"`
	HeadEnd      string `json:"head_end,omitempty"`
	DirtyAtStart bool   `json:"dirty_at_start"`
	Source       string `json:"source,omitempty"`
}
type Payload struct {
	Summary     Summary `json:"summary"`
	Files       []File  `json:"files"`
	Repo        *Repo   `json:"repo,omitempty"`
	GeneratedAt string  `json:"generated_at,omitempty"`
}
type Snapshot struct {
	Version         int               `json:"version"`
	TurnID          string            `json:"turnId"`
	CWD             string            `json:"cwd"`
	RepoRoot        string            `json:"repoRoot,omitempty"`
	StartedAt       string            `json:"startedAt"`
	HeadStart       string            `json:"headStart,omitempty"`
	DirtyAtStart    bool              `json:"dirtyAtStart"`
	DirtyPathsStart []string          `json:"dirtyPathsStart"`
	StartHashes     map[string]string `json:"startHashes"`
}
type CloseResult struct {
	Proof  *Payload `json:"proof,omitempty"`
	Found  bool     `json:"found"`
	Reason string   `json:"reason,omitempty"`
}

type Tracker struct{ Now func() time.Time }

func (t *Tracker) Start(cwd, turnID string) error {
	if err := t.cleanup(cwd); err != nil { /* cleanup is best effort */
	}
	dir := turnsDir(cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	repoRoot, _ := runGit(cwd, "rev-parse", "--show-toplevel")
	repoRoot = firstLine(repoRoot)
	dirty := []string{}
	if repoRoot != "" {
		dirty = dirtyPaths(repoRoot)
	}
	hashes := map[string]string{}
	for _, rel := range dirty {
		if hash, ok := fileHash(filepath.Join(repoRoot, filepath.FromSlash(rel))); ok {
			hashes[rel] = hash
		}
	}
	absRepo := ""
	if repoRoot != "" {
		resolvedRepo, absErr := filepath.Abs(repoRoot)
		if absErr == nil {
			absRepo = resolvedRepo
		}
	}
	snapshot := Snapshot{Version: 1, TurnID: turnID, CWD: absCWD, RepoRoot: absRepo, StartedAt: formatTime(t.now()), DirtyAtStart: len(dirty) > 0, DirtyPathsStart: dirty, StartHashes: hashes}
	if repoRoot != "" {
		snapshot.HeadStart = headSHA(repoRoot)
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(snapshotPath(cwd, turnID), data, 0o600)
}

func (t *Tracker) Close(cwd, turnID string) CloseResult {
	path := snapshotPath(cwd, turnID)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return CloseResult{Found: false, Reason: "turn_snapshot_missing"}
	}
	if err != nil {
		_ = os.Remove(path)
		return CloseResult{Found: false, Reason: "turn_snapshot_corrupt"}
	}
	defer os.Remove(path)
	var snapshot Snapshot
	if json.Unmarshal(data, &snapshot) != nil {
		return CloseResult{Found: false, Reason: "turn_snapshot_corrupt"}
	}
	var payload *Payload
	if snapshot.DirtyAtStart {
		payload = t.manifestProof(snapshot)
	} else {
		payload = t.gitProof(snapshot)
	}
	if payload == nil {
		return CloseResult{Found: true, Reason: "no_git_repository"}
	}
	return CloseResult{Proof: payload, Found: true}
}

func Bytes(value *Payload) int {
	if value == nil {
		return 2
	}
	data, _ := json.Marshal(value)
	return len(data)
}

func (t *Tracker) manifestProof(snapshot Snapshot) *Payload {
	if snapshot.RepoRoot == "" {
		return nil
	}
	candidates := append(append([]string{}, snapshot.DirtyPathsStart...), dirtyPaths(snapshot.RepoRoot)...)
	seen := map[string]bool{}
	files := []File{}
	for _, path := range candidates {
		if isInternalPath(path) {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		startHash := snapshot.StartHashes[path]
		endHash, _ := fileHash(filepath.Join(snapshot.RepoRoot, filepath.FromSlash(path)))
		if startHash == endHash {
			continue
		}
		status := "modified"
		if startHash == "" && endHash != "" {
			status = "added"
		}
		if startHash != "" && endHash == "" {
			status = "deleted"
		}
		file := File{Path: path, Status: status}
		if endHash != "" {
			file.Excerpt = excerpt(snapshot.RepoRoot, path, "")
		}
		files = append(files, file)
	}
	return &Payload{Summary: Summary{FilesChanged: len(files)}, Files: files, Repo: &Repo{VCS: "git", HeadStart: snapshot.HeadStart, HeadEnd: headSHA(snapshot.RepoRoot), DirtyAtStart: true, Source: "manifest_fallback"}, GeneratedAt: formatTime(t.now())}
}

type counts struct{ additions, deletions int }

func (t *Tracker) gitProof(snapshot Snapshot) *Payload {
	if snapshot.RepoRoot == "" {
		return nil
	}
	headEnd := headSHA(snapshot.RepoRoot)
	nums := map[string]counts{}
	statuses := map[string]string{}
	mergeNumstat(nums, git(snapshot.RepoRoot, "diff", "--numstat"))
	mergeNumstat(nums, git(snapshot.RepoRoot, "diff", "--cached", "--numstat"))
	mergeStatus(statuses, git(snapshot.RepoRoot, "diff", "--name-status"))
	mergeStatus(statuses, git(snapshot.RepoRoot, "diff", "--cached", "--name-status"))
	rangeArg := ""
	if snapshot.HeadStart != "" && headEnd != "" && snapshot.HeadStart != headEnd {
		rangeArg = snapshot.HeadStart + ".." + headEnd
		mergeNumstat(nums, git(snapshot.RepoRoot, "diff", "--numstat", rangeArg))
		mergeStatus(statuses, git(snapshot.RepoRoot, "diff", "--name-status", rangeArg))
	}
	for _, path := range lines(git(snapshot.RepoRoot, "ls-files", "--others", "--exclude-standard")) {
		path = normalizePath(path)
		if _, ok := statuses[path]; !ok {
			statuses[path] = "added"
		}
		if _, ok := nums[path]; !ok {
			nums[path] = counts{}
		}
	}
	pathSet := map[string]bool{}
	for path := range nums {
		pathSet[path] = true
	}
	for path := range statuses {
		pathSet[path] = true
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		if isInternalPath(path) {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	files := make([]File, 0, len(paths))
	totalAdd, totalDel := 0, 0
	for _, path := range paths {
		c := nums[path]
		status := statuses[path]
		if status == "" {
			status = "modified"
		}
		files = append(files, File{Path: path, Status: status, Additions: c.additions, Deletions: c.deletions, Excerpt: excerpt(snapshot.RepoRoot, path, rangeArg)})
		totalAdd += c.additions
		totalDel += c.deletions
	}
	return &Payload{Summary: Summary{len(files), totalAdd, totalDel}, Files: files, Repo: &Repo{VCS: "git", HeadStart: snapshot.HeadStart, HeadEnd: headEnd, DirtyAtStart: snapshot.DirtyAtStart, Source: "git"}, GeneratedAt: formatTime(t.now())}
}

func (t *Tracker) cleanup(cwd string) error {
	entries, err := os.ReadDir(turnsDir(cwd))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now := t.now()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(turnsDir(cwd), entry.Name())
		data, readErr := os.ReadFile(path)
		var s Snapshot
		if readErr != nil || json.Unmarshal(data, &s) != nil {
			_ = os.Remove(path)
			continue
		}
		started, parseErr := time.Parse(time.RFC3339Nano, s.StartedAt)
		if parseErr != nil || now.Sub(started) > SnapshotTTL {
			_ = os.Remove(path)
		}
	}
	return nil
}
func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}
func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
func turnsDir(cwd string) string             { return filepath.Join(cwd, ".recallstack", "turns") }
func snapshotPath(cwd, turnID string) string { return filepath.Join(turnsDir(cwd), turnID+".json") }
func runGit(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
func git(cwd string, args ...string) string { out, _ := runGit(cwd, args...); return out }
func headSHA(root string) string            { return firstLine(git(root, "rev-parse", "HEAD")) }
func firstLine(value string) string {
	if at := strings.IndexByte(value, '\n'); at >= 0 {
		return value[:at]
	}
	return strings.TrimSpace(value)
}
func lines(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(value), "\n")
}
func normalizePath(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
}
func isInternalPath(value string) bool {
	path := strings.TrimPrefix(normalizePath(value), "./")
	return path == ".recallstack" || strings.HasPrefix(path, ".recallstack/")
}
func statusPath(line string) string {
	if strings.TrimSpace(line) == "" {
		return ""
	}
	tail := ""
	if len(line) > 3 {
		tail = strings.TrimSpace(line[3:])
	}
	if strings.Contains(tail, " -> ") {
		parts := strings.Split(tail, " -> ")
		tail = parts[len(parts)-1]
	} else if len(line) >= 1 && (line[0] == 'R' || line[0] == 'C') {
		parts := strings.Split(tail, "\t")
		tail = parts[len(parts)-1]
	}
	return normalizePath(tail)
}
func dirtyPaths(root string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, line := range lines(git(root, "status", "--porcelain")) {
		path := statusPath(line)
		if path != "" && !isInternalPath(path) && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}
func fileHash(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
}
func excerpt(root, path, rangeArg string) string {
	args := []string{"diff", "--unified=0"}
	if rangeArg != "" {
		args = append(args, rangeArg)
	}
	args = append(args, "--", path)
	out := git(root, args...)
	if out == "" {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return ""
		}
		out = string(data)
	}
	out = truncateCharacters(out, 1200)
	return out
}
func truncateCharacters(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
func mergeNumstat(into map[string]counts, output string) {
	for _, line := range lines(output) {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		path := normalizePath(strings.Join(parts[2:], "\t"))
		add, _ := strconv.Atoi(parts[0])
		del, _ := strconv.Atoi(parts[1])
		prev := into[path]
		into[path] = counts{prev.additions + add, prev.deletions + del}
	}
}
func mergeStatus(into map[string]string, output string) {
	for _, line := range lines(output) {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		code := strings.TrimSpace(parts[0])
		path := normalizePath(parts[len(parts)-1])
		status := "modified"
		if strings.HasPrefix(code, "A") {
			status = "added"
		} else if strings.HasPrefix(code, "D") {
			status = "deleted"
		} else if strings.HasPrefix(code, "R") {
			status = "renamed"
		}
		into[path] = status
	}
}
