// Package state owns every file Recallstack keeps on this computer. All of it
// lives under one root (~/.recallstack, or $RECALLSTACK_HOME), so nothing is
// written into the person's repositories.
package state

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

// Root is the directory that holds config, state, cache, holds and queues.
func Root() string {
	if value := strings.TrimSpace(os.Getenv("RECALLSTACK_HOME")); value != "" {
		if abs, err := filepath.Abs(value); err == nil {
			return abs
		}
		return value
	}
	return filepath.Join(Home(), ".recallstack")
}

// Home is the person's home folder, honouring $HOME so isolated runs work.
func Home() string {
	if value := strings.TrimSpace(os.Getenv("HOME")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("USERPROFILE")); value != "" {
		return value
	}
	home, _ := os.UserHomeDir()
	return home
}

func ConfigPath() string   { return filepath.Join(Root(), "config.json") }
func ProjectsPath() string { return filepath.Join(Root(), "projects.json") }
func MePath() string       { return filepath.Join(Root(), "state", "me.json") }
func SessionsDir() string  { return filepath.Join(Root(), "state", "sessions") }
func ChangesDir() string   { return filepath.Join(Root(), "state", "changes") }
func ImportsPath() string  { return filepath.Join(Root(), "state", "imports.json") }
func ImportOfferedPath() string {
	return filepath.Join(Root(), "state", "import-offered")
}
func ImportHintsPath() string { return filepath.Join(Root(), "state", "import-hints.json") }
func CaptureDir() string      { return filepath.Join(Root(), "state", "capture") }
func JobsDir() string         { return filepath.Join(Root(), "state", "jobs") }
func HoldsDir() string        { return filepath.Join(Root(), "holds") }
func CacheDir() string        { return filepath.Join(Root(), "cache") }
func PendingDir() string      { return filepath.Join(Root(), "pending-writes") }
func LogsDir() string         { return filepath.Join(Root(), "logs") }
func AgentSettingsPath() string {
	return filepath.Join(Root(), "agent-settings.json")
}

// SafeName turns an opaque id (an agent's session id) into a file name.
func SafeName(value string) string {
	value = strings.TrimSpace(value)
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, value)
	if clean == "" || clean != value || len(clean) > 120 || strings.HasPrefix(clean, ".") {
		sum := sha1.Sum([]byte(value))
		prefix := clean
		if len(prefix) > 40 {
			prefix = prefix[:40]
		}
		prefix = strings.TrimLeft(prefix, ".")
		return prefix + "-" + hex.EncodeToString(sum[:])[:12]
	}
	return clean
}

// ReadJSON decodes path into target. It reports whether the file existed and parsed.
func ReadJSON(path string, target any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, target) == nil
}

// WriteJSON writes atomically with owner-only permissions.
func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(data, '\n'))
}

// WriteFile writes atomically with owner-only permissions.
func WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".rs-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	_ = temporary.Chmod(0o600)
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if goruntime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	return os.Rename(name, path)
}

func Remove(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WithLock runs fn while holding a simple lock file next to path.
func WithLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lockPath := path + ".lock"
	deadline := time.Now().Add(5 * time.Second)
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
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for " + filepath.Base(path))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// AppendLog adds one JSON line to logs/{name}. Failures are ignored: logging
// must never change what a command or hook does.
func AppendLog(name string, entry map[string]any) {
	if err := os.MkdirAll(LogsDir(), 0o700); err != nil {
		return
	}
	value := map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano)}
	for key, item := range entry {
		value[key] = item
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(LogsDir(), name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(data, '\n'))
}
