// Package project recognises which Recallstack project a folder belongs to.
// Inside a git repo the key is the normalised origin URL, else the repo's
// top-level path. The key → project map lives in ~/.recallstack/projects.json.
package project

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/state"
)

// Folder describes where a command or session runs.
type Folder struct {
	CWD      string `json:"cwd"`
	InRepo   bool   `json:"in_repo"`
	RepoRoot string `json:"repo_root,omitempty"`
	Key      string `json:"key"`
	KeyKind  string `json:"key_kind"` // repo | path
	Name     string `json:"name"`     // suggested project name
	Remote   string `json:"remote,omitempty"`
}

// Entry is one key's project in projects.json.
type Entry struct {
	ProjectID  string `json:"project_id"`
	Name       string `json:"name"`
	Slug       string `json:"slug,omitempty"`
	KeyKind    string `json:"key_kind"`
	ResolvedAt string `json:"resolved_at"`
	Linked     bool   `json:"linked,omitempty"`
}

type mapFile struct {
	Keys map[string]Entry `json:"keys"`
}

// Detect finds the repo (without running git) and builds the folder key.
func Detect(cwd string) Folder {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	folder := Folder{CWD: cwd, Key: "path:" + cwd, KeyKind: "path", Name: filepath.Base(cwd)}
	root, gitDir := findRepo(cwd)
	if root == "" {
		return folder
	}
	folder.InRepo = true
	folder.RepoRoot = root
	folder.Name = filepath.Base(root)
	folder.Key = "path:" + root
	if url := originURL(gitDir); url != "" {
		folder.Remote = url
		if key := NormaliseRemote(url); key != "" {
			folder.Key = key
			folder.KeyKind = "repo"
		}
	}
	return folder
}

func findRepo(start string) (string, string) {
	cursor := start
	for {
		candidate := filepath.Join(cursor, ".git")
		if info, err := os.Stat(candidate); err == nil {
			if info.IsDir() {
				return cursor, candidate
			}
			if raw, err := os.ReadFile(candidate); err == nil {
				line := strings.TrimSpace(string(raw))
				if strings.HasPrefix(line, "gitdir:") {
					dir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
					if !filepath.IsAbs(dir) {
						dir = filepath.Join(cursor, dir)
					}
					return cursor, dir
				}
			}
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "", ""
		}
		cursor = parent
	}
}

// originURL reads [remote "origin"] url from the repo config (worktrees use
// the common dir's config).
func originURL(gitDir string) string {
	configPaths := []string{filepath.Join(gitDir, "config")}
	if raw, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(raw))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		configPaths = append([]string{filepath.Join(common, "config")}, configPaths...)
	}
	for _, path := range configPaths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		inOrigin := false
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "[") {
				inOrigin = regexp.MustCompile(`^\[remote\s+"origin"\]$`).MatchString(line)
				continue
			}
			if inOrigin {
				if key, value, ok := strings.Cut(line, "="); ok && strings.TrimSpace(key) == "url" {
					file.Close()
					return strings.TrimSpace(value)
				}
			}
		}
		file.Close()
	}
	return ""
}

var scpForm = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormaliseRemote implements contract section 12: strip scheme, user@, port
// and trailing / and .git; scp form host:path → host/path; lowercase the host.
func NormaliseRemote(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	var host, path string
	if index := strings.Index(url, "://"); index >= 0 {
		rest := url[index+3:]
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, path, _ = strings.Cut(rest, "/")
		if colon := strings.LastIndex(host, ":"); colon >= 0 {
			host = host[:colon]
		}
	} else if match := scpForm.FindStringSubmatch(url); match != nil {
		host, path = match[1], match[2]
	} else {
		return ""
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if path == "" {
		return host
	}
	return host + "/" + path
}

func load() mapFile {
	var value mapFile
	state.ReadJSON(state.ProjectsPath(), &value)
	if value.Keys == nil {
		value.Keys = map[string]Entry{}
	}
	return value
}

// Lookup returns the stored project for a folder key.
func Lookup(key string) (Entry, bool) {
	entry, ok := load().Keys[key]
	return entry, ok && entry.ProjectID != ""
}

// Save stores key → entry.
func Save(key string, entry Entry) error {
	return state.WithLock(state.ProjectsPath(), func() error {
		value := load()
		if entry.ResolvedAt == "" {
			entry.ResolvedAt = time.Now().UTC().Format(time.RFC3339)
		}
		value.Keys[key] = entry
		return state.WriteJSON(state.ProjectsPath(), value)
	})
}

// UpdateName renames every entry pointing at projectID.
func UpdateName(projectID, name, slug string) error {
	return state.WithLock(state.ProjectsPath(), func() error {
		value := load()
		for key, entry := range value.Keys {
			if entry.ProjectID == projectID {
				entry.Name = name
				if slug != "" {
					entry.Slug = slug
				}
				value.Keys[key] = entry
			}
		}
		return state.WriteJSON(state.ProjectsPath(), value)
	})
}

// ErrNoProject means the folder is outside a repo and was never named.
var ErrNoProject = errors.New("this folder isn't a named project yet")

type projectBody struct {
	Project struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Slug    string `json:"slug"`
		Created bool   `json:"created"`
	} `json:"project"`
	MovedRecords int `json:"moved_records"`
}

// Resolve returns the folder's project, asking the server on first sight of a
// repo. Outside a repo it returns ErrNoProject unless `project use` named it.
func Resolve(ctx context.Context, svc *remote.Service, folder Folder) (Entry, bool, error) {
	if entry, ok := Lookup(folder.Key); ok {
		return entry, false, nil
	}
	if !folder.InRepo {
		return Entry{}, false, ErrNoProject
	}
	if !svc.SignedIn() {
		return Entry{Name: folder.Name, KeyKind: folder.KeyKind}, false, errors.New("not signed in")
	}
	var body projectBody
	err := svc.Do(ctx, remote.Request{Method: http.MethodPost, Path: "/v1/projects/resolve", Body: map[string]any{
		"repo_key": folder.Key, "key_kind": folder.KeyKind, "suggested_name": folder.Name,
	}}, &body)
	if err != nil {
		return Entry{Name: folder.Name, KeyKind: folder.KeyKind}, false, err
	}
	entry := Entry{ProjectID: body.Project.ID, Name: body.Project.Name, Slug: body.Project.Slug, KeyKind: folder.KeyKind}
	if entry.ProjectID == "" {
		return entry, false, errors.New("the server returned no project")
	}
	_ = Save(folder.Key, entry)
	return entry, body.Project.Created, nil
}

// Link points folder's key at the named project (`project use <name>`).
func Link(ctx context.Context, svc *remote.Service, folder Folder, name string) (Entry, int, bool, error) {
	var body projectBody
	err := svc.Do(ctx, remote.Request{Method: http.MethodPost, Path: "/v1/projects/link", Body: map[string]any{
		"key": folder.Key, "key_kind": folder.KeyKind, "name": name,
	}}, &body)
	if err != nil {
		return Entry{}, 0, false, err
	}
	entry := Entry{ProjectID: body.Project.ID, Name: body.Project.Name, Slug: body.Project.Slug, KeyKind: folder.KeyKind, Linked: true}
	if err := Save(folder.Key, entry); err != nil {
		return entry, body.MovedRecords, body.Project.Created, err
	}
	return entry, body.MovedRecords, body.Project.Created, nil
}

// Rename changes the project's name on the server and in projects.json.
func Rename(ctx context.Context, svc *remote.Service, projectID, name string) (Entry, error) {
	var body projectBody
	err := svc.Do(ctx, remote.Request{Method: http.MethodPatch, Path: "/v1/projects/" + projectID, Body: map[string]any{"name": name}}, &body)
	if err != nil {
		return Entry{}, err
	}
	_ = UpdateName(projectID, body.Project.Name, body.Project.Slug)
	return Entry{ProjectID: body.Project.ID, Name: body.Project.Name, Slug: body.Project.Slug}, nil
}
