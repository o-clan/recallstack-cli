package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	keyring "github.com/zalando/go-keyring"
)

const (
	DefaultBaseURL = "https://api.recallstack.com"
	KeyringService = "Recallstack CLI"
)

type Profile struct {
	AccessToken         string `json:"accessToken,omitempty"`
	RefreshToken        string `json:"refreshToken,omitempty"`
	APIKey              string `json:"apiKey,omitempty"`
	TokenStorage        string `json:"tokenStorage,omitempty"`
	ActiveWorkspaceID   string `json:"activeWorkspaceId,omitempty"`
	ActiveWorkspaceSlug string `json:"activeWorkspaceSlug,omitempty"`
	ActiveWorkspaceName string `json:"activeWorkspaceName,omitempty"`
}

type Config struct {
	Profile
	BaseURL          string `json:"baseUrl,omitempty"`
	EffectiveBaseURL string `json:"effectiveBaseUrl"`
}

type ProfilePatch struct {
	AccessToken         *string
	RefreshToken        *string
	APIKey              *string
	TokenStorage        *string
	ActiveWorkspaceID   *string
	ActiveWorkspaceSlug *string
	ActiveWorkspaceName *string
}

type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeProject Scope = "project"
)

type Resolved struct {
	Scope      Scope
	ScopeRoot  string
	Path       string
	GlobalPath string
}

type SecureStore interface {
	Get(service, account string) (string, error)
	Set(service, account, value string) error
	Delete(service, account string) error
}

type systemSecureStore struct{}

func (systemSecureStore) Get(service, account string) (string, error) {
	return keyring.Get(service, account)
}

func (systemSecureStore) Set(service, account, value string) error {
	if err := keyring.Set(service, account, value); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	// go-keyring encodes macOS values with a private prefix. Overwrite the
	// value in the raw format used by all released Node CLI versions so both
	// clients can read credentials during the migration window.
	command := exec.Command("/usr/bin/security", "-i")
	command.Stdin = strings.NewReader(macOSKeychainInput(service, account, value))
	return command.Run()
}

func macOSKeychainInput(service, account, value string) string {
	quote := func(input string) string {
		return "'" + strings.ReplaceAll(input, "'", `'\''`) + "'"
	}
	return "add-generic-password -U -s " + quote(service) + " -a " + quote(account) + " -w " + quote(value) + "\n"
}

func (systemSecureStore) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

type Store struct {
	HomeDir     string
	SecureStore SecureStore
	GOOS        string
}

func NewStore() *Store {
	home, _ := os.UserHomeDir()
	return &Store{HomeDir: home, SecureStore: systemSecureStore{}, GOOS: runtime.GOOS}
}

func (s *Store) homeDir() string {
	if strings.TrimSpace(s.HomeDir) != "" {
		return s.HomeDir
	}
	home, _ := os.UserHomeDir()
	return home
}

func (s *Store) globalPath() string {
	return filepath.Join(s.homeDir(), ".recallstack", "config.json")
}

func normalizeURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func normalizeValue(value string) string {
	return strings.TrimSpace(value)
}

func (s *Store) Resolve(cwd string) Resolved {
	if strings.TrimSpace(cwd) == "" {
		cwd, _ = os.Getwd()
	}
	root := findProjectRoot(cwd, s.homeDir())
	if root == "" {
		return Resolved{Scope: ScopeGlobal, ScopeRoot: s.homeDir(), Path: s.globalPath(), GlobalPath: s.globalPath()}
	}
	return Resolved{
		Scope: ScopeProject, ScopeRoot: root,
		Path: filepath.Join(root, ".recallstack", "config.json"), GlobalPath: s.globalPath(),
	}
}

func (s *Store) ResolveScope(cwd string, scope Scope) Resolved {
	if scope == ScopeGlobal {
		return Resolved{Scope: ScopeGlobal, ScopeRoot: s.homeDir(), Path: s.globalPath(), GlobalPath: s.globalPath()}
	}
	return s.Resolve(cwd)
}

func findProjectRoot(start, home string) string {
	cursor, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	homeAbs, _ := filepath.Abs(home)
	for {
		if cursor != homeAbs && (fileExists(filepath.Join(cursor, ".recallstack", "config.json")) || fileExists(filepath.Join(cursor, ".recallstack", "workspace.json"))) {
			return cursor
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return ""
		}
		cursor = parent
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

type globalFile struct {
	BaseURL  string             `json:"baseUrl,omitempty"`
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

type projectFile struct {
	BaseURL string `json:"baseUrl,omitempty"`
}

func readJSON(path string, target any) bool {
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, target) != nil {
		return false
	}
	return true
}

func rawString(value json.RawMessage) string {
	var out string
	if json.Unmarshal(value, &out) != nil {
		return ""
	}
	return out
}

func profileFromRaw(value json.RawMessage) Profile {
	var fields map[string]json.RawMessage
	if json.Unmarshal(value, &fields) != nil {
		return Profile{}
	}
	return normalizeProfile(Profile{
		AccessToken: rawString(fields["accessToken"]), RefreshToken: rawString(fields["refreshToken"]),
		APIKey: rawString(fields["apiKey"]), TokenStorage: rawString(fields["tokenStorage"]),
		ActiveWorkspaceID: rawString(fields["activeWorkspaceId"]), ActiveWorkspaceSlug: rawString(fields["activeWorkspaceSlug"]),
		ActiveWorkspaceName: rawString(fields["activeWorkspaceName"]),
	})
}

func readGlobal(path string) globalFile {
	var root map[string]json.RawMessage
	if !readJSON(path, &root) {
		return globalFile{Profiles: map[string]Profile{}}
	}
	out := globalFile{BaseURL: normalizeURL(rawString(root["baseUrl"])), Profiles: map[string]Profile{}}
	var rawProfiles map[string]json.RawMessage
	_ = json.Unmarshal(root["profiles"], &rawProfiles)
	for key, rawProfile := range rawProfiles {
		key = normalizeURL(key)
		profile := profileFromRaw(rawProfile)
		if key != "" && !profile.empty() {
			out.Profiles[key] = profile
		}
	}
	return out
}

func normalizeProfile(p Profile) Profile {
	p.AccessToken = normalizeValue(p.AccessToken)
	p.RefreshToken = normalizeValue(p.RefreshToken)
	p.APIKey = normalizeValue(p.APIKey)
	if p.TokenStorage != "keychain" {
		p.TokenStorage = ""
	}
	p.ActiveWorkspaceID = normalizeValue(p.ActiveWorkspaceID)
	p.ActiveWorkspaceSlug = normalizeValue(p.ActiveWorkspaceSlug)
	p.ActiveWorkspaceName = normalizeValue(p.ActiveWorkspaceName)
	return p
}

func (p Profile) empty() bool {
	return p == (Profile{})
}

func (s *Store) Load(cwd string) (Config, error) {
	resolved := s.Resolve(cwd)
	global := readGlobal(resolved.GlobalPath)
	baseURL := global.BaseURL
	if resolved.Scope == ScopeProject {
		var project map[string]json.RawMessage
		if readJSON(resolved.Path, &project) && normalizeURL(rawString(project["baseUrl"])) != "" {
			baseURL = normalizeURL(rawString(project["baseUrl"]))
		}
	}
	effective := normalizeURL(baseURL)
	if effective == "" {
		effective = DefaultBaseURL
	}
	profile, err := s.resolveSecrets(effective, global.Profiles[effective])
	if err != nil {
		return Config{}, err
	}
	return Config{Profile: profile, BaseURL: normalizeURL(baseURL), EffectiveBaseURL: effective}, nil
}

func (s *Store) resolveSecrets(baseURL string, profile Profile) (Profile, error) {
	profile = normalizeProfile(profile)
	if profile.TokenStorage != "keychain" || s.SecureStore == nil || !s.secureMode() {
		return profile, nil
	}
	for _, item := range tokenFields() {
		if *item.value(&profile) != "" {
			continue
		}
		value, err := s.SecureStore.Get(KeyringService, KeyringAccount(baseURL, item.name))
		if err == nil {
			*item.value(&profile) = normalizeValue(value)
			continue
		}
		if !errors.Is(err, keyring.ErrNotFound) {
			return Profile{}, fmt.Errorf("read %s from the system secure store: %w", item.name, err)
		}
	}
	return normalizeProfile(profile), nil
}

func KeyringAccount(baseURL, tokenName string) string {
	sum := sha256.Sum256([]byte(baseURL))
	return "recallstack:" + hex.EncodeToString(sum[:])[:32] + ":" + tokenName
}

type tokenField struct {
	name  string
	value func(*Profile) *string
}

func tokenFields() []tokenField {
	return []tokenField{
		{name: "accessToken", value: func(p *Profile) *string { return &p.AccessToken }},
		{name: "refreshToken", value: func(p *Profile) *string { return &p.RefreshToken }},
		{name: "apiKey", value: func(p *Profile) *string { return &p.APIKey }},
	}
}

func (s *Store) secureMode() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("RECALLSTACK_TOKEN_STORE")))
	return mode != "file"
}

func (s *Store) persistProfile(baseURL string, profile Profile) Profile {
	profile = normalizeProfile(profile)
	if !s.secureMode() || s.SecureStore == nil {
		return profile
	}
	hasToken := profile.AccessToken != "" || profile.RefreshToken != "" || profile.APIKey != ""
	secureOK := true
	deleteFailed := false
	for _, item := range tokenFields() {
		value := *item.value(&profile)
		var err error
		if value == "" {
			err = s.SecureStore.Delete(KeyringService, KeyringAccount(baseURL, item.name))
		} else {
			err = s.SecureStore.Set(KeyringService, KeyringAccount(baseURL, item.name), value)
		}
		if err != nil {
			secureOK = false
			if value == "" {
				deleteFailed = true
			}
		}
	}
	if deleteFailed && !hasToken {
		profile.TokenStorage = ""
		return profile
	}
	if !secureOK && hasToken {
		profile.TokenStorage = ""
		return profile
	}
	profile.AccessToken = ""
	profile.RefreshToken = ""
	profile.APIKey = ""
	if hasToken || profile.TokenStorage == "keychain" {
		profile.TokenStorage = "keychain"
	}
	return profile
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := restrictFilePermissions(tmpName); err != nil {
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
	return os.Rename(tmpName, path)
}

func withFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lockPath := path + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if err := restrictFilePermissions(lockPath); err != nil {
				_ = lock.Close()
				_ = os.Remove(lockPath)
				return err
			}
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
			return errors.New("timed out waiting for Recallstack config lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) SaveBaseURLOverride(cwd string, baseURL string) error {
	resolved := s.Resolve(cwd)
	baseURL = normalizeURL(baseURL)
	if resolved.Scope == ScopeProject {
		return writeJSON(resolved.Path, projectFile{BaseURL: baseURL})
	}
	return withFileLock(resolved.GlobalPath, func() error {
		global := readGlobal(resolved.GlobalPath)
		global.BaseURL = baseURL
		return writeJSON(resolved.GlobalPath, global)
	})
}

func applyPatch(profile Profile, patch ProfilePatch) Profile {
	if patch.AccessToken != nil {
		profile.AccessToken = *patch.AccessToken
	}
	if patch.RefreshToken != nil {
		profile.RefreshToken = *patch.RefreshToken
	}
	if patch.APIKey != nil {
		profile.APIKey = *patch.APIKey
	}
	if patch.TokenStorage != nil {
		profile.TokenStorage = *patch.TokenStorage
	}
	if patch.ActiveWorkspaceID != nil {
		profile.ActiveWorkspaceID = *patch.ActiveWorkspaceID
	}
	if patch.ActiveWorkspaceSlug != nil {
		profile.ActiveWorkspaceSlug = *patch.ActiveWorkspaceSlug
	}
	if patch.ActiveWorkspaceName != nil {
		profile.ActiveWorkspaceName = *patch.ActiveWorkspaceName
	}
	return normalizeProfile(profile)
}

func (s *Store) UpdateProfile(baseURL string, patch ProfilePatch) error {
	baseURL = normalizeURL(baseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return withFileLock(s.globalPath(), func() error {
		global := readGlobal(s.globalPath())
		current, err := s.resolveSecrets(baseURL, global.Profiles[baseURL])
		if err != nil {
			return err
		}
		next := s.persistProfile(baseURL, applyPatch(current, patch))
		if next.empty() {
			delete(global.Profiles, baseURL)
		} else {
			global.Profiles[baseURL] = next
		}
		return writeJSON(s.globalPath(), global)
	})
}

func matchesExpected(current Profile, expected ProfilePatch) bool {
	checks := []struct {
		want *string
		got  string
	}{
		{expected.AccessToken, current.AccessToken}, {expected.RefreshToken, current.RefreshToken},
		{expected.APIKey, current.APIKey}, {expected.TokenStorage, current.TokenStorage},
		{expected.ActiveWorkspaceID, current.ActiveWorkspaceID}, {expected.ActiveWorkspaceSlug, current.ActiveWorkspaceSlug},
		{expected.ActiveWorkspaceName, current.ActiveWorkspaceName},
	}
	for _, check := range checks {
		if check.want != nil && normalizeValue(*check.want) != check.got {
			return false
		}
	}
	return true
}

func (s *Store) UpdateProfileIfCurrent(baseURL string, expected, patch ProfilePatch) (bool, error) {
	baseURL = normalizeURL(baseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	updated := false
	err := withFileLock(s.globalPath(), func() error {
		global := readGlobal(s.globalPath())
		current, err := s.resolveSecrets(baseURL, global.Profiles[baseURL])
		if err != nil {
			return err
		}
		if !matchesExpected(current, expected) {
			return nil
		}
		next := s.persistProfile(baseURL, applyPatch(current, patch))
		if next.empty() {
			delete(global.Profiles, baseURL)
		} else {
			global.Profiles[baseURL] = next
		}
		updated = true
		return writeJSON(s.globalPath(), global)
	})
	return updated, err
}

func String(value string) *string { return &value }
