package state

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Me is the cached signed-in person, used for offline lines ("until Peter answers").
type Me struct {
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	FirstName  string `json:"first_name"`
	TZ         string `json:"tz,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
}

func LoadMe() Me {
	var me Me
	ReadJSON(MePath(), &me)
	return me
}

func SaveMe(me Me) error { return WriteJSON(MePath(), me) }

// PersonName is the first name for "{person}" lines, with a neutral fallback.
func PersonName() string {
	if name := strings.TrimSpace(LoadMe().FirstName); name != "" {
		return name
	}
	return "the person"
}

// Session is what a session-start hook records about an agent session.
type Session struct {
	Agent          string `json:"agent"`
	SessionID      string `json:"session_id"`
	ProjectID      string `json:"project_id,omitempty"`
	ProjectName    string `json:"project_name,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	CWDKey         string `json:"cwd_key,omitempty"`
	RepoRoot       string `json:"repo_root,omitempty"`
	Source         string `json:"source,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	AgentPID       int    `json:"agent_pid,omitempty"`
	V              int    `json:"v,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	LastSeenAt     string `json:"last_seen_at,omitempty"`
	LastBriefAt    string `json:"last_brief_at,omitempty"`
	EndedAt        string `json:"ended_at,omitempty"`
	NeedsYou       int    `json:"needs_you,omitempty"` // Review count last shown to the person
}

func SessionPath(agent, sessionID string) string {
	return filepath.Join(SessionsDir(), agent+"-"+SafeName(sessionID)+".json")
}

func PIDPath(pid int) string {
	return filepath.Join(SessionsDir(), "pid-"+strconv.Itoa(pid)+".json")
}

func LoadSession(agent, sessionID string) (Session, bool) {
	var value Session
	ok := ReadJSON(SessionPath(agent, sessionID), &value)
	return value, ok && value.SessionID != ""
}

func SaveSession(value Session) error {
	value.LastSeenAt = time.Now().UTC().Format(time.RFC3339)
	return WriteJSON(SessionPath(value.Agent, value.SessionID), value)
}

// PIDLink ties an agent process to the session it is running, so commands the
// agent runs can find their session by walking up the process tree.
type PIDLink struct {
	PID       int    `json:"pid"`
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd,omitempty"`
	Started   string `json:"started,omitempty"`
	WrittenAt string `json:"written_at"`
}

func LoadPIDLink(pid int) (PIDLink, bool) {
	var value PIDLink
	ok := ReadJSON(PIDPath(pid), &value)
	return value, ok && value.SessionID != ""
}

// NewestSession returns the most recent session for agent (any agent when
// empty) whose folder matches cwd, seen within maxAge.
func NewestSession(agent, cwd string, maxAge time.Duration) (Session, bool) {
	entries, err := os.ReadDir(SessionsDir())
	if err != nil {
		return Session{}, false
	}
	var best Session
	var bestAt time.Time
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, "pid-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if agent != "" && !strings.HasPrefix(name, agent+"-") {
			continue
		}
		var value Session
		if !ReadJSON(filepath.Join(SessionsDir(), name), &value) || value.SessionID == "" || value.EndedAt != "" {
			continue
		}
		if cwd != "" && !sameFolder(value, cwd) {
			continue
		}
		at, _ := time.Parse(time.RFC3339, value.LastSeenAt)
		if at.IsZero() || time.Since(at) > maxAge {
			continue
		}
		if at.After(bestAt) {
			best, bestAt = value, at
		}
	}
	return best, !bestAt.IsZero()
}

func sameFolder(value Session, cwd string) bool {
	cwd = filepath.Clean(cwd)
	for _, root := range []string{value.RepoRoot, value.CWD} {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Hold is the local hold file. The before-tool hook reads only this.
type Hold struct {
	ClientRef   string `json:"client_ref"`
	Ref         string `json:"ref,omitempty"`
	AgainstRef  string `json:"against_ref"`
	AgainstType string `json:"against_type,omitempty"`
	Plan        string `json:"plan"`
	Person      string `json:"person"`
	SessionID   string `json:"session_id"`
	Agent       string `json:"agent"`
	ProjectID   string `json:"project_id,omitempty"`
	CreatedAt   string `json:"created_at"`
	Synced      bool   `json:"synced"`
}

// DisplayRef is the ref the agent should resolve: the server's, else the client's.
func (h Hold) DisplayRef() string {
	if h.Ref != "" {
		return h.Ref
	}
	return h.ClientRef
}

func HoldPath(sessionID string) string {
	return filepath.Join(HoldsDir(), SafeName(sessionID)+".json")
}

func LoadHold(sessionID string) (Hold, bool) {
	var value Hold
	ok := ReadJSON(HoldPath(sessionID), &value)
	return value, ok && value.AgainstRef != ""
}

func SaveHold(value Hold) error { return WriteJSON(HoldPath(value.SessionID), value) }

func RemoveHold(sessionID string) { Remove(HoldPath(sessionID)) }

// Change is one undoable change this CLI made, kept per session for `undo`.
type Change struct {
	ChangeID  string `json:"change_id"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref,omitempty"`
	What      string `json:"what,omitempty"`
	At        string `json:"at"`
	PendingID string `json:"pending_id,omitempty"`
	Undone    bool   `json:"undone,omitempty"`
}

type changeLog struct {
	Changes []Change `json:"changes"`
}

// ChangesPath is state/changes/{session_id}.json, or by-hand.json.
func ChangesPath(sessionID string) string {
	if sessionID == "" {
		return filepath.Join(ChangesDir(), "by-hand.json")
	}
	return filepath.Join(ChangesDir(), SafeName(sessionID)+".json")
}

func LoadChanges(sessionID string) []Change {
	var log changeLog
	ReadJSON(ChangesPath(sessionID), &log)
	return log.Changes
}

func AddChange(sessionID string, value Change) error {
	path := ChangesPath(sessionID)
	return WithLock(path, func() error {
		var log changeLog
		ReadJSON(path, &log)
		if value.At == "" {
			value.At = time.Now().UTC().Format(time.RFC3339)
		}
		log.Changes = append(log.Changes, value)
		if len(log.Changes) > 200 {
			log.Changes = log.Changes[len(log.Changes)-200:]
		}
		return WriteJSON(path, log)
	})
}

// UpdateChange rewrites the entry with changeID.
func UpdateChange(sessionID, changeID string, update func(*Change)) error {
	path := ChangesPath(sessionID)
	return WithLock(path, func() error {
		var log changeLog
		ReadJSON(path, &log)
		for index := range log.Changes {
			if log.Changes[index].ChangeID == changeID {
				update(&log.Changes[index])
			}
		}
		return WriteJSON(path, log)
	})
}

// BriefCachePath is cache/brief-{project_id}.txt ("none" outside a project).
func BriefCachePath(projectID string) string {
	if projectID == "" {
		projectID = "none"
	}
	return filepath.Join(CacheDir(), "brief-"+SafeName(projectID)+".txt")
}

func SaveBriefCache(projectID, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return WriteFile(BriefCachePath(projectID), []byte(text))
}

// LoadBriefCache returns the cached brief and when it was written.
func LoadBriefCache(projectID string) (string, time.Time, bool) {
	path := BriefCachePath(projectID)
	data, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return "", time.Time{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return string(data), time.Time{}, true
	}
	return string(data), info.ModTime(), true
}

// ListSessions returns every recorded session, newest first.
func ListSessions() []Session {
	entries, err := os.ReadDir(SessionsDir())
	if err != nil {
		return nil
	}
	values := []Session{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, "pid-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		var value Session
		if ReadJSON(filepath.Join(SessionsDir(), name), &value) && value.SessionID != "" {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].LastSeenAt > values[j].LastSeenAt })
	return values
}
