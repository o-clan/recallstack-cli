package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

type fakeSecureStore struct {
	values      map[string]string
	err         error
	getErr      error
	deleteErr   error
	deleteCalls int
}

func (f *fakeSecureStore) key(service, account string) string { return service + "\x00" + account }
func (f *fakeSecureStore) Get(service, account string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	if f.err != nil {
		return "", f.err
	}
	value, ok := f.values[f.key(service, account)]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (f *fakeSecureStore) Set(service, account, value string) error {
	if f.err != nil {
		return f.err
	}
	f.values[f.key(service, account)] = value
	return nil
}
func (f *fakeSecureStore) Delete(service, account string) error {
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.err != nil {
		return f.err
	}
	delete(f.values, f.key(service, account))
	return nil
}

func TestLoadResolvesProjectOverrideAndEndpointProfile(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "file")
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "repo")
	nested := filepath.Join(project, "a", "b")
	if err := os.MkdirAll(filepath.Join(project, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	global := `{
  "baseUrl": "https://api.recallstack.com/",
  "profiles": {
    "https://api.recallstack.com": {"accessToken":"prod-token"},
    "http://localhost:3001": {"apiKey":"local-key","activeWorkspaceSlug":"local"}
  }
}`
	if err := os.MkdirAll(filepath.Join(home, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".recallstack", "config.json"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".recallstack", "config.json"), []byte(`{"baseUrl":"http://localhost:3001/"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &Store{HomeDir: home, GOOS: "linux"}
	resolved := store.Resolve(nested)
	if resolved.Scope != ScopeProject || resolved.ScopeRoot != project {
		t.Fatalf("unexpected resolution: %+v", resolved)
	}
	cfg, err := store.Load(nested)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveBaseURL != "http://localhost:3001" || cfg.APIKey != "local-key" || cfg.AccessToken != "" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestKeyringStorageUsesCompatibleServiceAndAccount(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "")
	home := t.TempDir()
	secure := &fakeSecureStore{values: map[string]string{}}
	store := &Store{HomeDir: home, SecureStore: secure, GOOS: "linux"}
	baseURL := "https://api.recallstack.com"
	if err := store.UpdateProfile(baseURL, ProfilePatch{
		AccessToken: String("access"), RefreshToken: String("refresh"), ActiveWorkspaceID: String("ws-1"),
	}); err != nil {
		t.Fatal(err)
	}

	for name, expected := range map[string]string{"accessToken": "access", "refreshToken": "refresh"} {
		account := KeyringAccount(baseURL, name)
		if len(account) != len("recallstack:")+32+1+len(name) {
			t.Fatalf("unexpected account %q", account)
		}
		if got := secure.values[secure.key(KeyringService, account)]; got != expected {
			t.Fatalf("%s = %q", name, got)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, ".recallstack", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	profile := disk["profiles"].(map[string]any)[baseURL].(map[string]any)
	if _, ok := profile["accessToken"]; ok {
		t.Fatal("access token leaked to JSON")
	}
	if profile["tokenStorage"] != "keychain" {
		t.Fatalf("unexpected profile: %#v", profile)
	}
	cfg, err := store.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "access" || cfg.RefreshToken != "refresh" || cfg.ActiveWorkspaceID != "ws-1" {
		t.Fatalf("unexpected loaded config: %+v", cfg)
	}
}

func TestKeyringFailureFallsBackToFileAndConditionalUpdateProtectsNewTokens(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "")
	home := t.TempDir()
	store := &Store{HomeDir: home, SecureStore: &fakeSecureStore{values: map[string]string{}, err: errors.New("unavailable")}, GOOS: "linux"}
	baseURL := DefaultBaseURL
	if err := store.UpdateProfile(baseURL, ProfilePatch{AccessToken: String("old"), RefreshToken: String("refresh-old")}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.Load(home)
	if cfg.AccessToken != "old" || cfg.TokenStorage != "" {
		t.Fatalf("expected file fallback, got %+v", cfg)
	}
	restricted, err := filePermissionsRestricted(filepath.Join(home, ".recallstack", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !restricted {
		t.Fatal("fallback config permissions are too broad")
	}
	if err := store.UpdateProfile(baseURL, ProfilePatch{AccessToken: String("new"), RefreshToken: String("refresh-new")}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.UpdateProfileIfCurrent(baseURL,
		ProfilePatch{RefreshToken: String("refresh-old")},
		ProfilePatch{AccessToken: String(""), RefreshToken: String("")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("stale conditional update replaced newer tokens")
	}
	cfg, _ = store.Load(home)
	if cfg.AccessToken != "new" || cfg.RefreshToken != "refresh-new" {
		t.Fatalf("new credentials lost: %+v", cfg)
	}
}

func TestSecureStoreReadFailureCannotMutateOrDeleteCredentials(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "")
	home := t.TempDir()
	baseURL := DefaultBaseURL
	secure := &fakeSecureStore{values: map[string]string{}}
	secure.values[secure.key(KeyringService, KeyringAccount(baseURL, "accessToken"))] = "access"
	secure.values[secure.key(KeyringService, KeyringAccount(baseURL, "refreshToken"))] = "refresh"
	if err := os.MkdirAll(filepath.Join(home, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".recallstack", "config.json")
	original := `{"profiles":{"https://api.recallstack.com":{"tokenStorage":"keychain","activeWorkspaceId":"ws-old"}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{HomeDir: home, SecureStore: secure, GOOS: "linux"}
	secure.getErr = errors.New("secure store temporarily unavailable")
	if err := store.UpdateProfile(baseURL, ProfilePatch{ActiveWorkspaceID: String("ws-new")}); err == nil {
		t.Fatal("expected secure-store read failure")
	}
	if secure.deleteCalls != 0 {
		t.Fatalf("unrelated update attempted %d secure-store deletes", secure.deleteCalls)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != original {
		t.Fatalf("config changed after secure-store read failure: %s", raw)
	}
	secure.getErr = nil
	cfg, err := store.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "access" || cfg.RefreshToken != "refresh" || cfg.ActiveWorkspaceID != "ws-old" {
		t.Fatalf("credentials changed after failed update: %+v", cfg)
	}
}

func TestMacOSCompatibilityCommandKeepsSecretOutOfArguments(t *testing.T) {
	secret := "token'; echo exposed"
	input := macOSKeychainInput("Recallstack CLI", "account", secret)
	if !strings.Contains(input, "-w 'token'\\''; echo exposed'") {
		t.Fatalf("secret was not safely quoted for security input: %q", input)
	}
}

func TestSecureStorageDefaultsAcrossReleasePlatforms(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "")
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			secure := &fakeSecureStore{values: map[string]string{}}
			store := &Store{HomeDir: t.TempDir(), SecureStore: secure, GOOS: goos}
			if err := store.UpdateProfile(DefaultBaseURL, ProfilePatch{AccessToken: String("access")}); err != nil {
				t.Fatal(err)
			}
			account := KeyringAccount(DefaultBaseURL, "accessToken")
			if secure.values[secure.key(KeyringService, account)] != "access" {
				t.Fatalf("secure storage was not used by default on %s", goos)
			}
		})
	}
}

func TestMalformedProfileFieldDoesNotDiscardValidConfig(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "file")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"baseUrl":"https://example.test","profiles":{"https://example.test":{"accessToken":42,"refreshToken":"valid-refresh"},"https://other.test":{"apiKey":"other"}}}`
	if err := os.WriteFile(filepath.Join(home, ".recallstack", "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{HomeDir: home, GOOS: "linux"}
	cfg, err := store.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveBaseURL != "https://example.test" || cfg.AccessToken != "" || cfg.RefreshToken != "valid-refresh" {
		t.Fatalf("valid config was discarded: %+v", cfg)
	}
}

func TestFailedKeyringDeleteCannotResurrectClearedCredentials(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "keychain")
	home := t.TempDir()
	baseURL := DefaultBaseURL
	secure := &fakeSecureStore{values: map[string]string{}}
	secure.values[secure.key(KeyringService, KeyringAccount(baseURL, "accessToken"))] = "access"
	secure.values[secure.key(KeyringService, KeyringAccount(baseURL, "refreshToken"))] = "refresh"
	if err := os.MkdirAll(filepath.Join(home, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"profiles":{"https://api.recallstack.com":{"tokenStorage":"keychain","activeWorkspaceId":"ws"}}}`
	if err := os.WriteFile(filepath.Join(home, ".recallstack", "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{HomeDir: home, SecureStore: secure, GOOS: "linux"}
	cfg, _ := store.Load(home)
	if cfg.AccessToken != "access" {
		t.Fatalf("fixture did not load keyring token: %+v", cfg)
	}
	secure.deleteErr = errors.New("delete denied")
	if err := store.UpdateProfile(baseURL, ProfilePatch{AccessToken: String(""), RefreshToken: String("")}); err != nil {
		t.Fatal(err)
	}
	secure.deleteErr = nil
	cfg, _ = store.Load(home)
	if cfg.AccessToken != "" || cfg.RefreshToken != "" || cfg.TokenStorage != "" {
		t.Fatalf("cleared credentials resurrected: %+v", cfg)
	}
}

func TestFileModeDoesNotReadKeyringProfile(t *testing.T) {
	t.Setenv("RECALLSTACK_TOKEN_STORE", "file")
	home := t.TempDir()
	baseURL := DefaultBaseURL
	secure := &fakeSecureStore{values: map[string]string{
		KeyringService + "\x00" + KeyringAccount(baseURL, "accessToken"): "must-not-load",
	}}
	if err := os.MkdirAll(filepath.Join(home, ".recallstack"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"profiles":{"https://api.recallstack.com":{"tokenStorage":"keychain"}}}`
	if err := os.WriteFile(filepath.Join(home, ".recallstack", "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{HomeDir: home, SecureStore: secure, GOOS: "darwin"}
	cfg, err := store.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "" {
		t.Fatalf("file mode unexpectedly loaded keyring token: %+v", cfg)
	}
}
