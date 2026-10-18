package hooks

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/o-clan/recallstack-cli/internal/state"
)

// CursorRulePath is the one file Recallstack writes inside a repo, and only for Cursor.
const CursorRulePath = ".cursor/rules/recallstack-brief.mdc"

// writeCursorBrief writes the always-apply rule and keeps it out of git.
func writeCursorBrief(repoRoot, brief string) error {
	if repoRoot == "" {
		return nil
	}
	path := filepath.Join(repoRoot, filepath.FromSlash(CursorRulePath))
	body := "---\ndescription: Recallstack brief, refreshed at the end of each turn\nalwaysApply: true\n---\n" + strings.TrimSpace(brief) + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	return excludeFromGit(repoRoot, CursorRulePath)
}

// excludeFromGit adds pattern to .git/info/exclude (the common dir for worktrees).
func excludeFromGit(repoRoot, pattern string) error {
	gitPath := filepath.Join(repoRoot, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return nil
	}
	gitDir := gitPath
	if !info.IsDir() {
		raw, err := os.ReadFile(gitPath)
		if err != nil {
			return nil
		}
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "gitdir:") {
			return nil
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(repoRoot, gitDir)
		}
		if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			dir := strings.TrimSpace(string(common))
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(gitDir, dir)
			}
			gitDir = dir
		}
	}
	excludePath := filepath.Join(gitDir, "info", "exclude")
	current, _ := os.ReadFile(excludePath)
	for _, line := range strings.Split(string(current), "\n") {
		if strings.TrimSpace(line) == pattern || strings.TrimSpace(line) == "/"+pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}
	text := string(current)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += "/" + pattern + "\n"
	return os.WriteFile(excludePath, []byte(text), 0o644)
}

const (
	briefBlockStart = "<!-- recallstack:brief:start -->"
	briefBlockEnd   = "<!-- recallstack:brief:end -->"
)

// CopilotInstructionsPath is the user-level Copilot instructions file whose
// managed block carries the brief.
func CopilotInstructionsPath() string {
	return filepath.Join(state.Home(), ".copilot", "copilot-instructions.md")
}

func writeCopilotBrief(brief string) error {
	path := CopilotInstructionsPath()
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return nil
	}
	current, _ := os.ReadFile(path)
	return os.WriteFile(path, []byte(ReplaceBlock(string(current), briefBlockStart, briefBlockEnd, strings.TrimSpace(brief))), 0o644)
}

// AntigravityBriefPaths are the skill files whose brief section is refreshed at Stop.
func AntigravityBriefPaths() []string {
	return []string{filepath.Join(state.Home(), ".gemini", "antigravity-cli", "plugins", "recallstack", "skills", "recallstack", "SKILL.md")}
}

func writeAntigravityBrief(brief string) error {
	for _, path := range AntigravityBriefPaths() {
		current, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		_ = os.WriteFile(path, []byte(ReplaceBlock(string(current), briefBlockStart, briefBlockEnd, strings.TrimSpace(brief))), 0o644)
	}
	return nil
}

// ReplaceBlock puts body between start and end markers, adding them when missing.
func ReplaceBlock(current, start, end, body string) string {
	block := start + "\n" + body + "\n" + end
	from := strings.Index(current, start)
	to := strings.Index(current, end)
	if from >= 0 && to > from {
		return current[:from] + block + current[to+len(end):]
	}
	if strings.TrimSpace(current) == "" {
		return block + "\n"
	}
	if !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	return current + "\n" + block + "\n"
}

// RemoveBlock takes the managed block out again.
func RemoveBlock(current, start, end string) (string, bool) {
	from := strings.Index(current, start)
	to := strings.Index(current, end)
	if from < 0 || to < from {
		return current, false
	}
	next := strings.TrimRight(current[:from], "\n") + current[to+len(end):]
	next = strings.TrimSpace(next)
	if next == "" {
		return "", true
	}
	return next + "\n", true
}

func BriefBlockMarkers() (string, string) { return briefBlockStart, briefBlockEnd }
