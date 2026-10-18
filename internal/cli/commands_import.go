package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/o-clan/recallstack-cli/internal/hooks"
	"github.com/o-clan/recallstack-cli/internal/integrations"
	"github.com/o-clan/recallstack-cli/internal/project"
	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/secrets"
	"github.com/o-clan/recallstack-cli/internal/state"
)

// ruleFile is one file import reads.
type ruleFile struct {
	Abs       string // absolute path ("" for pasted rules)
	Display   string // ~/… for home files, repo-relative for repo files
	Scope     string // everywhere | project | path
	ScopePath string // for path scope
	PointsTo  string
	Lines     []string
	SHA       string
	ChangedAt time.Time
	Agent     string
	Kept      []numberedLine // lines sent to the reader (new or changed, secrets held back)
	Secrets   int
	Folder    bool // a rules folder (.cursor/rules)
}

type numberedLine struct {
	N    int
	Text string
}

type importState struct {
	Files map[string]struct {
		SHA        string   `json:"sha256"`
		LineHashes []string `json:"line_hashes"`
		ImportedAt string   `json:"imported_at"`
	} `json:"files"`
}

func loadImportState() importState {
	var value importState
	state.ReadJSON(state.ImportsPath(), &value)
	if value.Files == nil {
		value.Files = map[string]struct {
			SHA        string   `json:"sha256"`
			LineHashes []string `json:"line_hashes"`
			ImportedAt string   `json:"imported_at"`
		}{}
	}
	return value
}

var pointerLine = regexp.MustCompile(`^@(\S+)$`)
var pullIn = regexp.MustCompile(`(?m)^\s*@(~?[\w./-]+\.md)\s*$`)

const maxRuleLines = 2000

func lineHash(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:8])
}

func fileAgent(display string) string {
	base := filepath.Base(display)
	switch {
	case base == "CLAUDE.md" || base == "CLAUDE.local.md":
		return "claude"
	case display == "~/.codex/AGENTS.md":
		return "codex"
	case base == ".cursorrules" || strings.HasSuffix(base, ".mdc") || display == "Cursor user rules":
		return "cursor"
	case base == "copilot-instructions.md":
		return "copilot"
	case base == "GEMINI.md":
		return "antigravity"
	}
	return ""
}

// readRuleFile loads a file, stripping Recallstack's own managed blocks.
func readRuleFile(abs, display, scope, scopePath string) (*ruleFile, error) {
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	info, _ := os.Stat(abs)
	text := string(raw)
	for _, markers := range [][2]string{{"<!-- recallstack:instructions:start -->", "<!-- recallstack:instructions:end -->"}, {"<!-- recallstack:brief:start -->", "<!-- recallstack:brief:end -->"}} {
		if next, removed := hooks.RemoveBlock(text, markers[0], markers[1]); removed {
			text = next
		}
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > maxRuleLines {
		return nil, fmt.Errorf("over %d lines", maxRuleLines)
	}
	sum := sha256.Sum256(raw)
	file := &ruleFile{Abs: abs, Display: display, Scope: scope, ScopePath: scopePath, Lines: lines, SHA: hex.EncodeToString(sum[:]), Agent: fileAgent(display)}
	if info != nil {
		file.ChangedAt = info.ModTime()
	}
	nonEmpty := []string{}
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmpty = append(nonEmpty, strings.TrimSpace(line))
		}
	}
	if len(nonEmpty) == 1 {
		if match := pointerLine.FindStringSubmatch(nonEmpty[0]); match != nil {
			file.PointsTo = match[1]
		}
	}
	return file, nil
}

func homeDisplay(abs string) string {
	home := state.Home()
	if strings.HasPrefix(abs, home+string(os.PathSeparator)) {
		return "~" + abs[len(home):]
	}
	return abs
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".next": true, "target": true, ".venv": true, "venv": true, "__pycache__": true, ".recallstack": true, ".claude": true, ".codex": true}

var repoRuleNames = map[string]bool{"AGENTS.md": true, "CLAUDE.md": true, "CLAUDE.local.md": true, "GEMINI.md": true, ".cursorrules": true}

// discoverRuleFiles finds the files spec section 11 reads.
func discoverRuleFiles(folder project.Folder) []*ruleFile {
	files := []*ruleFile{}
	seen := map[string]bool{}
	add := func(abs, display, scope, scopePath string) *ruleFile {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil
		}
		if seen[resolved] {
			return nil
		}
		file, err := readRuleFile(abs, display, scope, scopePath)
		if err != nil {
			return nil
		}
		seen[resolved] = true
		files = append(files, file)
		return file
	}
	home := state.Home()
	if file := add(filepath.Join(home, ".claude", "CLAUDE.md"), "~/.claude/CLAUDE.md", "everywhere", ""); file != nil {
		for _, match := range pullIn.FindAllStringSubmatch(strings.Join(file.Lines, "\n"), -1) {
			target := match[1]
			if strings.HasPrefix(target, "~/") {
				target = filepath.Join(home, target[2:])
			} else if !filepath.IsAbs(target) {
				target = filepath.Join(home, ".claude", target)
			}
			add(target, homeDisplay(target), "everywhere", "")
		}
	}
	add(filepath.Join(home, ".codex", "AGENTS.md"), "~/.codex/AGENTS.md", "everywhere", "")
	if !folder.InRepo {
		return files
	}
	root := folder.RepoRoot
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			if path != root && (skipDirs[entry.Name()] || strings.Count(rel, string(os.PathSeparator)) > 5) && entry.Name() != ".cursor" && entry.Name() != ".github" {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.Dir(rel)
		scope, scopePath := "project", ""
		name := entry.Name()
		switch {
		case repoRuleNames[name]:
			if dir != "." {
				scope, scopePath = "path", filepath.ToSlash(dir)
			}
		case name == "copilot-instructions.md" && filepath.Base(dir) == ".github":
			if parent := filepath.Dir(dir); parent != "." {
				scope, scopePath = "path", filepath.ToSlash(parent)
			}
		case strings.HasSuffix(name, ".mdc") && strings.HasSuffix(filepath.ToSlash(dir), ".cursor/rules"):
			if strings.HasPrefix(name, "recallstack") {
				return nil
			}
			if parent := filepath.Dir(filepath.Dir(dir)); parent != "." {
				scope, scopePath = "path", filepath.ToSlash(parent)
			}
			raw, _ := os.ReadFile(path)
			if folder := globsFolder(mdcGlobs(string(raw))); folder != "" && scope == "project" {
				scope, scopePath = "path", folder
			}
		default:
			return nil
		}
		add(path, filepath.ToSlash(rel), scope, scopePath)
		return nil
	})
	return files
}

func mdcGlobs(text string) string {
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	end := strings.Index(text[3:], "---")
	if end < 0 {
		return ""
	}
	for _, line := range strings.Split(text[3:3+end], "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "globs" {
			value = strings.Trim(strings.TrimSpace(value), `"'[]`)
			return value
		}
	}
	return ""
}

// globsFolder turns a rule's globs ("apps/web/**", "src/a/*.tsx, src/b/**")
// into the folder they share, because a path scope is a folder in the repo.
// Globs with no common literal folder ("**/*.tsx") give "" (the whole project).
func globsFolder(globs string) string {
	common := []string(nil)
	for i, glob := range strings.Split(globs, ",") {
		glob = strings.Trim(strings.TrimSpace(glob), `"'`)
		if glob == "" {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(filepath.ToSlash(glob), "./"), "/")
		literal := []string{}
		for j, part := range parts {
			if part == "" || strings.ContainsAny(part, "*?[{") || j == len(parts)-1 && strings.Contains(part, ".") {
				break
			}
			literal = append(literal, part)
		}
		if i == 0 || common == nil {
			common = literal
			continue
		}
		n := 0
		for n < len(common) && n < len(literal) && common[n] == literal[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, "/")
}

// prepare marks the lines worth reading: new or changed since the last
// import, with secret-looking lines held back.
func prepare(files []*ruleFile, previous importState) {
	for _, file := range files {
		known := map[string]bool{}
		if file.Abs != "" {
			for _, hash := range previous.Files[file.Abs].LineHashes {
				known[hash] = true
			}
		}
		file.Kept, file.Secrets = nil, 0
		if file.PointsTo != "" {
			continue
		}
		for index, line := range file.Lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || known[lineHash(trimmed)] {
				continue
			}
			if secrets.Contains(trimmed) {
				file.Secrets++
				continue
			}
			file.Kept = append(file.Kept, numberedLine{N: index + 1, Text: line})
		}
	}
}

func (r *runtimeServices) ruleFilesForOffer() []*ruleFile {
	return discoverRuleFiles(project.Detect(r.cwd))
}

type importCandidate struct {
	Kind      string   `json:"kind"`
	What      string   `json:"what"`
	Why       *string  `json:"why"`
	InsteadOf []string `json:"instead_of"`
	Where     struct {
		Scope string  `json:"scope"`
		Path  *string `json:"path"`
	} `json:"where"`
	Quote   string `json:"quote"`
	Sources []struct {
		Path      string `json:"path"`
		LineStart int    `json:"line_start"`
		LineEnd   int    `json:"line_end"`
	} `json:"sources"`
	AlreadyKnownRef *string `json:"already_known_ref"`
}

type importReading struct {
	Candidates []importCandidate `json:"candidates"`
	Skipped    struct {
		FileMaps             int `json:"file_maps"`
		BuildAndTestCommands int `json:"build_and_test_commands"`
		ToolAndModelSettings int `json:"tool_and_model_settings"`
	} `json:"skipped"`
	DuplicatesAcrossFiles int `json:"duplicates_across_files"`
}

const importSchema = `{
  "type": "object", "additionalProperties": false,
  "required": ["candidates", "skipped", "duplicates_across_files"],
  "properties": {
    "candidates": { "type": "array", "items": {
      "type": "object", "additionalProperties": false,
      "required": ["kind", "what", "why", "instead_of", "where", "quote", "sources", "already_known_ref"],
      "properties": {
        "kind": { "type": "string", "enum": ["goal", "decision", "question", "taste"] },
        "what": { "type": "string" },
        "why": { "type": ["string", "null"] },
        "instead_of": { "type": "array", "items": { "type": "string" } },
        "where": { "type": "object", "additionalProperties": false, "required": ["scope", "path"], "properties": {
          "scope": { "type": "string", "enum": ["everywhere", "project", "path"] }, "path": { "type": ["string", "null"] } } },
        "quote": { "type": "string" },
        "sources": { "type": "array", "items": { "type": "object", "additionalProperties": false, "required": ["path", "line_start", "line_end"],
          "properties": { "path": { "type": "string" }, "line_start": { "type": "integer" }, "line_end": { "type": "integer" } } } },
        "already_known_ref": { "type": ["string", "null"] }
      } } },
    "skipped": { "type": "object", "additionalProperties": false, "required": ["file_maps", "build_and_test_commands", "tool_and_model_settings"],
      "properties": { "file_maps": { "type": "integer" }, "build_and_test_commands": { "type": "integer" }, "tool_and_model_settings": { "type": "integer" } } },
    "duplicates_across_files": { "type": "integer" }
  }
}`

func importPrompt(person, projectName string, files []*ruleFile, existing []map[string]any) string {
	var body strings.Builder
	for _, file := range files {
		if len(file.Kept) == 0 {
			continue
		}
		scope := file.Scope
		if file.Scope == "path" {
			scope = "path " + file.ScopePath
		}
		fmt.Fprintf(&body, "=== %s (scope: %s) ===\n", file.Display, scope)
		for _, line := range file.Kept {
			fmt.Fprintf(&body, "%d| %s\n", line.N, line.Text)
		}
		body.WriteString("\n")
	}
	known, _ := json.Marshal(existing)
	return fmt.Sprintf(`These are rule files %[1]s wrote for their coding agents. Turn the intent and taste in them into thoughts, so every agent gets them.

The test for every line: would it still be true if the code were rewritten from scratch? If yes, it's intent or taste: keep it. If it only describes how today's code or tools work, skip it and count it.

Keep as:
- taste: how writing, interfaces, code or replies should be ("Plain English", "Don't add tests unless I ask").
- decision: a choice, with its reason in "why" and losing options in "instead_of" when the file gives them ("Use pnpm, not npm").
- goal: what the project is for or what comes first.
- question: something the file marks as undecided.

Skip, and count in "skipped":
- file_maps: folder layouts, where modules live, architecture descriptions.
- build_and_test_commands: how to install, build, run, test, lint or deploy.
- tool_and_model_settings: model names, MCP servers, permissions, hook and editor config, env var names, instructions about one agent's own tools.
A rule that names a tool can still be taste ("Use Docker for experiments, not host installs" is kept; "Run docker compose up -d db before tests" is skipped). Headings and blank lines aren't counted. Condense long procedures into one or a few thoughts.

For each thought: "what" is one plain sentence (at most 300 characters); "quote" is the exact lines it came from (at most 500 characters); "sources" gives the file path exactly as shown in the === header and the line numbers; "where" follows the file's scope (a path-scoped file gives scope "path" with that path). The same rule in several files is one thought with every source; count those in "duplicates_across_files". If a thought has the same meaning as one %[1]s already told Recallstack (listed below), set "already_known_ref" to its ref.

Already told Recallstack (project %[2]s):
%[3]s

The files (line number, then the line):
%[4]s
Return only JSON that matches the schema.`, person, projectName, string(known), body.String())
}

func (r *runtimeServices) runImport(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"paste", "yes", "review", "preview", "json"}))
	if err != nil {
		return err
	}
	mode := ""
	switch {
	case parsed.bools["yes"]:
		mode = "import"
	case parsed.bools["review"]:
		mode = "review"
	case parsed.bools["preview"]:
		mode = "preview"
	}
	return r.importFlow(parsed.positionals, parsed.bools["paste"], mode, parsed.bools["json"], false)
}

func (r *runtimeServices) importAtInstall() error {
	_ = state.WriteFile(state.ImportOfferedPath(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"))
	return r.importFlow(nil, false, "", false, true)
}

func (r *runtimeServices) importFlow(paths []string, paste bool, mode string, asJSON, atInstall bool) error {
	out := r.deps.Stdout
	if !r.svc.SignedIn() {
		return &ExitError{Code: 1, Message: "Sign in first, so the thoughts have somewhere to go:  recallstack login"}
	}
	ctx := context.Background()
	tc := r.thoughtContext(ctx)
	folder := tc.folder
	var files []*ruleFile
	if len(paths) > 0 {
		for _, path := range paths {
			abs, _ := filepath.Abs(path)
			display, scope, scopePath := homeDisplay(abs), "everywhere", ""
			if folder.InRepo && strings.HasPrefix(abs, folder.RepoRoot+string(os.PathSeparator)) {
				rel, _ := filepath.Rel(folder.RepoRoot, abs)
				display, scope = filepath.ToSlash(rel), "project"
				if dir := filepath.Dir(rel); dir != "." && filepath.Base(dir) != ".github" && !strings.HasSuffix(filepath.ToSlash(dir), ".cursor/rules") {
					scope, scopePath = "path", filepath.ToSlash(dir)
				}
			}
			file, err := readRuleFile(abs, display, scope, scopePath)
			if err != nil {
				return &ExitError{Code: 1, Message: fmt.Sprintf("Can't read %s: %v", path, err)}
			}
			files = append(files, file)
		}
	} else {
		files = discoverRuleFiles(folder)
	}
	if paste {
		fmt.Fprintln(r.deps.Stderr, "Paste Cursor's user rules, then press Ctrl+D.")
		raw, _ := io.ReadAll(r.deps.Stdin)
		if strings.TrimSpace(string(raw)) != "" {
			lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
			sum := sha256.Sum256(raw)
			files = append(files, &ruleFile{Display: "Cursor user rules", Scope: "everywhere", Lines: lines, SHA: hex.EncodeToString(sum[:]), Agent: "cursor", ChangedAt: time.Now()})
		}
	}
	if len(files) == 0 {
		fmt.Fprintln(out, "No rule files found in this repo or your home folder.")
		return nil
	}
	previous := loadImportState()
	prepare(files, previous)
	fresh := 0
	for _, file := range files {
		fresh += len(file.Kept)
	}
	projectName := tc.projectName()
	connected := r.connectedTargets()
	if !asJSON {
		fmt.Fprintln(out, "Found rules you wrote for your agents")
		for _, file := range files {
			where := "everywhere"
			switch file.Scope {
			case "project":
				where = projectName
			case "path":
				where = file.ScopePath + " in " + projectName
			}
			detail := fmt.Sprintf("%d lines", countLines(file.Lines))
			if countLines(file.Lines) == 1 {
				detail = "1 line"
			}
			if file.PointsTo != "" {
				detail = "points to " + file.PointsTo
			}
			fmt.Fprintf(out, "  %-34s %-24s %s\n", file.Display, where, detail)
		}
		if connected["cursor"] && !paste {
			fmt.Fprintln(out, "  Cursor's user rules live inside Cursor, not in a file.")
			fmt.Fprintln(out, "  Paste them in later:  recallstack import --paste")
		}
		fmt.Fprintln(out)
	}
	if fresh == 0 {
		oldest := ""
		for _, file := range files {
			if entry, ok := previous.Files[file.Abs]; ok && (oldest == "" || entry.ImportedAt < oldest) {
				oldest = entry.ImportedAt
			}
		}
		fmt.Fprintf(out, "Nothing new in %d files since %s.\n", len(files), dateLabel(oldest, false))
		return nil
	}

	var existing []map[string]any
	{
		listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var response struct {
			Records []map[string]any `json:"records"`
		}
		query := "/v1/records?states=in_force&limit=500"
		if id, ok := tc.projectID().(string); ok {
			query += "&project_id=" + id
		}
		if err := r.svc.Do(listCtx, remote.Request{Method: http.MethodGet, Path: query}, &response); err == nil {
			for _, record := range response.Records {
				existing = append(existing, map[string]any{"ref": record["ref"], "kind": record["kind"], "what": record["what"]})
			}
		}
		cancel()
	}

	readers := []string{}
	for _, candidate := range []string{"claude", "codex", "copilot"} {
		if _, err := exec.LookPath(candidate); err == nil {
			readers = append(readers, candidate)
		}
	}
	if len(readers) == 0 {
		return &ExitError{Code: 1, Message: "Importing needs Claude Code, Codex or Copilot to read the files, and none of them can run here."}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	go func() {
		if _, ok := <-stop; ok {
			cancelRead()
		}
	}()
	prompt := importPrompt(r.person(), projectName, files, existing)
	var reading importReading
	var reader string
	started := time.Now()
	var readErr error
	for _, candidate := range readers {
		if !asJSON {
			fmt.Fprintf(out, "Reading them with %s, on this computer. Your files stay as they are.\n", agentDisplay(candidate))
		}
		attemptCtx, cancel := context.WithTimeout(readCtx, 5*time.Minute)
		raw, err := hooks.RunHeadless(attemptCtx, candidate, prompt, importSchema)
		cancel()
		if readCtx.Err() != nil {
			fmt.Fprintln(out, "Stopped. Nothing was sent.")
			return &ExitError{Code: 130}
		}
		if err == nil {
			if err = json.Unmarshal(raw, &reading); err == nil {
				reader = candidate
				break
			}
		}
		readErr = err
		if !asJSON {
			fmt.Fprintf(out, "%s couldn't read them (%s).\n", agentDisplay(candidate), firstLine(err.Error()))
		}
	}
	if reader == "" {
		return &ExitError{Code: 1, Message: fmt.Sprintf("Importing needs Claude Code, Codex or Copilot to read the files, and none of them could: %v", readErr)}
	}
	readable := 0
	for _, file := range files {
		if file.PointsTo == "" {
			readable++
		}
	}
	candidates, payloadCandidates := normalizeImport(reading.Candidates, files)
	heldBack := 0
	for _, file := range files {
		heldBack += file.Secrets
	}
	if !asJSON {
		fmt.Fprintf(out, "✓ Read %d files in %ds\n\n", readable, int(time.Since(started).Seconds()+0.5))
		r.printImportPreview(candidates, projectName, reading, heldBack, len(existing) > 0)
	}
	if mode == "preview" {
		if asJSON {
			return writeJSON(out, map[string]any{"candidates": payloadCandidates, "skipped": reading.Skipped, "held_back_secrets": heldBack, "reader": reader})
		}
		return nil
	}
	if len(payloadCandidates) == 0 {
		fmt.Fprintln(out, "Nothing in them reads as intent or taste, so there's nothing to import.")
		r.saveImportState(files, previous)
		return nil
	}
	if mode == "" {
		if !r.interactive() {
			fmt.Fprintln(out, "Run recallstack import --yes to import them, or --review to look them over first.")
			return nil
		}
		fmt.Fprint(out, "Import them?  a all   r review them first   s skip   [A/r/s] ")
		answer, _ := bufio.NewReader(r.deps.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "a", "all", "y", "yes":
			mode = "import"
		case "r", "review":
			mode = "review"
		default:
			fmt.Fprintln(out, "Skipped. Nothing was sent.")
			fmt.Fprintln(out, "Run recallstack import whenever you like.")
			return nil
		}
	}
	fileHashes := []string{}
	filesBody := []map[string]any{}
	for _, file := range files {
		fileHashes = append(fileHashes, file.SHA)
		filesBody = append(filesBody, map[string]any{
			"path": file.Display, "scope": file.Scope, "sha256": file.SHA, "lines": countLines(file.Lines),
			"changed_at": file.ChangedAt.UTC().Format(time.RFC3339), "points_to": nullable(file.PointsTo),
		})
	}
	sort.Strings(fileHashes)
	changeID := uuid7()
	body := map[string]any{
		"change_id": changeID, "mode": mode, "agent": reader, "project_id": tc.projectID(), "files": filesBody,
		"candidates":        payloadCandidates,
		"skipped":           map[string]any{"file_maps": reading.Skipped.FileMaps, "build_and_test_commands": reading.Skipped.BuildAndTestCommands, "tool_and_model_settings": reading.Skipped.ToolAndModelSettings},
		"held_back_secrets": heldBack,
	}
	var response struct {
		ImportID        string           `json:"import_id"`
		ChangeID        string           `json:"change_id"`
		Created         []map[string]any `json:"created"`
		AlreadyKnown    []string         `json:"already_known"`
		NoLongerInFiles []struct {
			Ref       string  `json:"ref"`
			What      string  `json:"what"`
			Path      string  `json:"path"`
			LineStart int     `json:"line_start"`
			Quote     *string `json:"quote"`
		} `json:"no_longer_in_files"`
		ReviewURL string `json:"review_url"`
	}
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := r.svc.Do(sendCtx, remote.Request{Method: http.MethodPost, Path: "/v1/imports", Body: body, IdempotencyKey: "import-" + sha1Text(fileHashes...)}, &response)
	if err != nil {
		return r.networkError(err)
	}
	r.saveImportState(files, previous)
	_ = state.AddChange("", state.Change{ChangeID: firstNonEmptyString(response.ChangeID, changeID), Kind: "import", What: fmt.Sprintf("%d imported thoughts", len(response.Created))})
	if asJSON {
		return writeJSON(out, response)
	}
	// The Thoughts app isn't out yet: point at recallstack review and why, not at a link.
	fmt.Fprintln(out)
	if mode == "review" {
		fmt.Fprintf(out, "%s waiting in review. They stay out of every brief until you keep them.\n", countThoughts(len(response.Created), "is", "are"))
		fmt.Fprintln(out, "Keep them, one key at a time:  recallstack review")
	} else {
		fmt.Fprintf(out, "✓ Imported %s. They count as your words, marked imported.\n", countThoughts(len(response.Created), "", ""))
		fmt.Fprintln(out, "Every agent gets them now, not only the one each file was written for.")
		if first := firstCreatedRef(response.Created); first != "" {
			fmt.Fprintf(out, "Look one over:  recallstack why %s\n", first)
		}
		fmt.Fprintln(out, "Take them all back:  recallstack undo")
	}
	// The server lists every earlier thought from these files that no candidate
	// matched, but only new and changed lines were read this time. A thought is
	// gone only when its words are no longer in its file.
	gone := response.NoLongerInFiles[:0]
	for _, item := range response.NoLongerInFiles {
		if item.Quote != nil && !stillInFile(files, item.Path, *item.Quote) {
			gone = append(gone, item)
		}
	}
	if len(gone) > 0 && r.interactive() {
		if len(gone) == 1 {
			fmt.Fprint(out, "1 imported thought isn't in its file anymore. Drop it too? [y/N] ")
		} else {
			fmt.Fprintf(out, "%d imported thoughts aren't in their files anymore. Drop them too? [y/N] ", len(gone))
		}
		answer, _ := bufio.NewReader(r.deps.Stdin).ReadString('\n')
		if strings.EqualFold(strings.TrimSpace(answer), "y") {
			for _, item := range gone {
				var ignored any
				id := uuid7()
				_, _ = r.svc.Send(ctx, remote.Write{Path: "/v1/records/" + item.Ref + "/drop", Label: "drop", ChangeID: id, IdempotencyKey: id,
					Body: map[string]any{"change_id": id, "project_id": tc.projectID(), "session_id": nil, "agent": "hand", "quote": "No longer in " + item.Path}}, &ignored)
				fmt.Fprintf(out, "Dropped %s · %s\n", item.Ref, sentence(firstSentence(item.What)))
			}
		}
	}
	return nil
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	if len(value) > 160 {
		value = value[:159] + "…"
	}
	return value
}

func countLines(lines []string) int {
	count := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func (r *runtimeServices) connectedTargets() map[string]bool {
	out := map[string]bool{}
	for _, scope := range []integrations.Scope{integrations.ScopeGlobal, integrations.ScopeLocal} {
		manager, err := r.integrationManager(scope)
		if err != nil {
			continue
		}
		for _, result := range manager.Detect(scope) {
			if result.Connected {
				out[string(result.Target)] = true
			}
		}
	}
	return out
}

// normalizeImport validates the reader's output against the files and builds
// the request candidates. Secrets are stripped again.
var listMarker = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?`)

// quoteText is the person's words from a rule file without the Markdown list
// markers around them, so "- Deploy only from main" quotes as "Deploy only from main".
func quoteText(value string) string {
	return strings.TrimSpace(listMarker.ReplaceAllString(strings.TrimSpace(value), ""))
}

// stillInFile reports whether a quote from an earlier import is still in its
// file (list markers, case and spacing aside). Parts held back as secrets are
// matched around.
// countThoughts is "1 thought" or "{n} thoughts", with the verb when one is given.
func countThoughts(n int, one, many string) string {
	if n == 1 {
		return strings.TrimSpace("1 thought " + one)
	}
	return strings.TrimSpace(fmt.Sprintf("%d thoughts %s", n, many))
}

func stillInFile(files []*ruleFile, display, quote string) bool {
	normalize := func(value string) string {
		return strings.Join(strings.Fields(strings.ToLower(quoteText(value))), " ")
	}
	for _, file := range files {
		if file.Display != display {
			continue
		}
		lines := make([]string, len(file.Lines))
		for index, line := range file.Lines {
			lines[index] = normalize(line)
		}
		text := strings.Join(lines, " ")
		for _, part := range strings.Split(quote, secrets.Marker) {
			part = strings.TrimSuffix(strings.TrimSpace(part), "…")
			if part = normalize(part); part != "" && !strings.Contains(text, part) {
				return false
			}
		}
		return true
	}
	return false
}

func normalizeImport(values []importCandidate, files []*ruleFile) ([]importCandidate, []map[string]any) {
	known := map[string]*ruleFile{}
	for _, file := range files {
		known[file.Display] = file
	}
	kept := []importCandidate{}
	payload := []map[string]any{}
	for _, value := range values {
		value.Kind = strings.ToLower(value.Kind)
		if !noteKinds[value.Kind] || value.Kind == "focus" || strings.TrimSpace(value.What) == "" {
			continue
		}
		value.What = secrets.StripString(strings.TrimSpace(value.What))
		if runes := []rune(value.What); len(runes) > 300 {
			value.What = string(runes[:299]) + "…"
		}
		value.Quote = secrets.StripString(quoteText(value.Quote))
		if runes := []rune(value.Quote); len(runes) > 500 {
			value.Quote = string(runes[:499]) + "…"
		}
		sources := []map[string]any{}
		for _, source := range value.Sources {
			if _, ok := known[source.Path]; ok && source.LineStart > 0 {
				end := source.LineEnd
				if end < source.LineStart {
					end = source.LineStart
				}
				sources = append(sources, map[string]any{"path": source.Path, "line_start": source.LineStart, "line_end": end})
			}
		}
		if len(sources) == 0 {
			continue
		}
		scope := value.Where.Scope
		if scope != "everywhere" && scope != "project" && scope != "path" {
			scope = "project"
		}
		var path any
		if scope == "path" && value.Where.Path != nil && *value.Where.Path != "" {
			path = *value.Where.Path
		} else if scope == "path" {
			scope = "project"
		}
		var why any
		if value.Why != nil && strings.TrimSpace(*value.Why) != "" {
			why = secrets.StripString(strings.TrimSpace(*value.Why))
		}
		insteadOf := []string{}
		for _, option := range value.InsteadOf {
			if option = strings.TrimSpace(secrets.StripString(option)); option != "" && len(insteadOf) < 5 {
				insteadOf = append(insteadOf, option)
			}
		}
		kept = append(kept, value)
		payload = append(payload, map[string]any{
			"kind": value.Kind, "what": value.What, "why": why, "instead_of": insteadOf,
			"where": map[string]any{"scope": scope, "path": path}, "quote": value.Quote, "sources": sources,
			"already_known_ref": value.AlreadyKnownRef,
		})
		if len(payload) == 300 {
			break
		}
	}
	return kept, payload
}

func (r *runtimeServices) printImportPreview(candidates []importCandidate, projectName string, reading importReading, heldBack int, haveExisting bool) {
	out := r.deps.Stdout
	fresh := []importCandidate{}
	known := 0
	for _, candidate := range candidates {
		if candidate.AlreadyKnownRef != nil && *candidate.AlreadyKnownRef != "" {
			known++
			continue
		}
		fresh = append(fresh, candidate)
	}
	fmt.Fprintf(out, "%s in your own words\n", countThoughts(len(fresh), "", ""))
	everywhere, here := []importCandidate{}, []importCandidate{}
	for _, candidate := range fresh {
		if candidate.Where.Scope == "everywhere" {
			everywhere = append(everywhere, candidate)
		} else {
			here = append(here, candidate)
		}
	}
	sort.SliceStable(everywhere, func(i, j int) bool { return everywhere[i].Kind == "taste" && everywhere[j].Kind != "taste" })
	if len(everywhere) > 0 {
		fmt.Fprintln(out, "  Everywhere")
		for index, candidate := range everywhere {
			if index == 4 {
				counts := map[string]int{}
				for _, rest := range everywhere[4:] {
					counts[rest.Kind]++
				}
				for _, kind := range []string{"taste", "decision", "goal", "question"} {
					if counts[kind] > 0 {
						fmt.Fprintf(out, "    +%d more %s\n", counts[kind], kind)
					}
				}
				break
			}
			fmt.Fprintf(out, "    %s%s\n", r.dot(candidate.Kind, true), candidate.What)
		}
	}
	if len(here) > 0 {
		fmt.Fprintf(out, "  %s\n", projectName)
		for index, candidate := range here {
			if index == 5 {
				fmt.Fprintf(out, "    +%d more\n", len(here)-5)
				break
			}
			line := fmt.Sprintf("    %s%s  %s", r.dot(candidate.Kind, true), strings.ToUpper(candidate.Kind[:1])+candidate.Kind[1:], candidate.What)
			if candidate.Why != nil && *candidate.Why != "" {
				line += "  why: " + *candidate.Why
			}
			fmt.Fprintln(out, line)
		}
	}
	if reading.DuplicatesAcrossFiles > 0 {
		fmt.Fprintf(out, "  %d rules were in more than one file, so each is kept once.\n", reading.DuplicatesAcrossFiles)
	}
	if known > 0 && haveExisting {
		fmt.Fprintf(out, "  %d you'd already told Recallstack, so they aren't repeated.\n", known)
	}
	skipped := reading.Skipped.FileMaps + reading.Skipped.BuildAndTestCommands + reading.Skipped.ToolAndModelSettings
	if skipped > 0 {
		fmt.Fprintf(out, "\nSkipped %d lines that describe the code or your tools, not what you want:\n", skipped)
		fmt.Fprintf(out, "  %d file maps · %d build and test commands · %d tool and model settings\n", reading.Skipped.FileMaps, reading.Skipped.BuildAndTestCommands, reading.Skipped.ToolAndModelSettings)
		fmt.Fprintln(out, "  Agents still read those files, so nothing is lost.")
	}
	switch {
	case heldBack == 1:
		fmt.Fprintln(out, "Held back 1 line that looked like a secret. It never left this computer.")
	case heldBack > 1:
		fmt.Fprintf(out, "Held back %d lines that looked like secrets. They never left this computer.\n", heldBack)
	}
	fmt.Fprintln(out)
}

func (r *runtimeServices) saveImportState(files []*ruleFile, previous importState) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, file := range files {
		if file.Abs == "" {
			continue
		}
		hashes := []string{}
		for _, line := range file.Lines {
			if strings.TrimSpace(line) != "" && !secrets.Contains(line) {
				hashes = append(hashes, lineHash(line))
			}
		}
		entry := previous.Files[file.Abs]
		entry.SHA, entry.LineHashes, entry.ImportedAt = file.SHA, hashes, now
		previous.Files[file.Abs] = entry
	}
	if err := state.WriteJSON(state.ImportsPath(), previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(r.deps.Stderr, "Couldn't save the import record:", err)
	}
}

func firstCreatedRef(created []map[string]any) string {
	for _, item := range created {
		if ref, ok := item["ref"].(string); ok && ref != "" {
			return ref
		}
	}
	return ""
}
