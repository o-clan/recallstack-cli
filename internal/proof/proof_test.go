package proof

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func TestCleanSnapshotCapturesChanges(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	excludeFile := filepath.Join(t.TempDir(), "empty-excludes")
	if err := os.WriteFile(excludeFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "config", "core.excludesFile", excludeFile)
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", "initial")
	tracker := Tracker{Now: func() time.Time { return time.Unix(1, 0) }}
	if err := tracker.Start(dir, "turn"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tracker.Close(dir, "turn")
	if !got.Found || got.Proof == nil {
		t.Fatalf("unexpected result: %#v", got)
	}
	if got.Proof.Summary.FilesChanged != 1 || got.Proof.Files[0].Path != "a.txt" || got.Proof.Files[0].Additions != 1 {
		t.Fatalf("unexpected proof: %#v", got.Proof)
	}
	if _, err := os.Stat(snapshotPath(dir, "turn")); !os.IsNotExist(err) {
		t.Fatalf("snapshot not removed: %v", err)
	}
}

func TestMissingAndCorruptSnapshots(t *testing.T) {
	dir := t.TempDir()
	tracker := Tracker{}
	if got := tracker.Close(dir, "missing"); got.Found || got.Reason != "turn_snapshot_missing" {
		t.Fatalf("unexpected missing: %#v", got)
	}
	if err := os.MkdirAll(turnsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath(dir, "bad"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tracker.Close(dir, "bad"); got.Found || got.Reason != "turn_snapshot_corrupt" {
		t.Fatalf("unexpected corrupt: %#v", got)
	}
}
