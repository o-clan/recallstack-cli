package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/project"
)

// currentProject resolves this folder's project, asking the server on first
// sight of a repo when signed in.
func (r *runtimeServices) currentProject(ctx context.Context) (project.Folder, project.Entry, bool) {
	folder := project.Detect(r.cwd)
	if entry, ok := project.Lookup(folder.Key); ok {
		return folder, entry, true
	}
	if !folder.InRepo || !r.svc.SignedIn() {
		return folder, project.Entry{}, false
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	entry, _, err := project.Resolve(resolveCtx, r.svc, folder)
	if err != nil || entry.ProjectID == "" {
		return folder, project.Entry{}, false
	}
	return folder, entry, true
}

func (r *runtimeServices) runProject(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"json"}))
	if err != nil {
		return err
	}
	ctx := context.Background()
	out := r.deps.Stdout
	if len(parsed.positionals) == 0 || parsed.positionals[0] == "show" {
		folder, entry, ok := r.currentProject(ctx)
		if parsed.bools["json"] {
			value := map[string]any{"folder": folder, "project": nil}
			if ok {
				value["project"] = entry
			}
			return writeJSON(out, value)
		}
		switch {
		case ok && folder.InRepo:
			fmt.Fprintf(out, "Project  %s (this repo)\n", entry.Name)
		case ok:
			fmt.Fprintf(out, "Project  %s (this folder)\n", entry.Name)
		case folder.InRepo && !r.svc.SignedIn():
			fmt.Fprintf(out, "Project  %s, recognised from this git repo once you sign in:  recallstack login\n", folder.Name)
		case folder.InRepo:
			fmt.Fprintf(out, "Project  %s, recognised from this git repo. Recallstack couldn't be reached to confirm it.\n", folder.Name)
		default:
			fmt.Fprintln(out, "This folder isn't a named project yet.")
			fmt.Fprintln(out, "Name it with  recallstack project use \"<name>\"")
		}
		return nil
	}
	switch parsed.positionals[0] {
	case "rename":
		name := strings.TrimSpace(strings.Join(parsed.positionals[1:], " "))
		if name == "" {
			return fmt.Errorf("usage: recallstack project rename <name>")
		}
		if !r.svc.SignedIn() {
			return &ExitError{Code: 1, Message: "Sign in first:  recallstack login"}
		}
		folder, entry, ok := r.currentProject(ctx)
		if !ok {
			if folder.InRepo {
				return &ExitError{Code: 1, Message: "Recallstack couldn't be reached to find this repo's project. Try again in a moment."}
			}
			return &ExitError{Code: 1, Message: "This folder isn't a named project yet. Name it with  recallstack project use \"<name>\""}
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		renamed, err := project.Rename(requestCtx, r.svc, entry.ProjectID, name)
		if err != nil {
			if api.Code(err) == "NAME_TAKEN" {
				return &ExitError{Code: 1, Message: fmt.Sprintf("You already have a project called %s. Pick another name.", name)}
			}
			return r.networkError(err)
		}
		fmt.Fprintf(out, "Renamed %s to %s. Refs and thoughts stay as they are.\n", entry.Name, renamed.Name)
		return nil
	case "use":
		name := strings.TrimSpace(strings.Join(parsed.positionals[1:], " "))
		if name == "" {
			return fmt.Errorf("usage: recallstack project use <name>")
		}
		if !r.svc.SignedIn() {
			return &ExitError{Code: 1, Message: "Sign in first:  recallstack login"}
		}
		folder := project.Detect(r.cwd)
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		entry, moved, created, err := project.Link(requestCtx, r.svc, folder, name)
		if err != nil {
			return r.networkError(err)
		}
		where := "This folder"
		if folder.InRepo {
			where = "This repo"
		}
		verb := "now belongs to"
		if created {
			verb = "now belongs to the new project"
		}
		fmt.Fprintf(out, "%s %s %s.\n", where, verb, entry.Name)
		if moved > 0 {
			fmt.Fprintf(out, "%d thoughts captured here earlier moved into it. They wait in Review.\n", moved)
		}
		return nil
	}
	return fmt.Errorf("unknown project command %q. Use rename or use", parsed.positionals[0])
}

// networkError turns transport and server failures into one plain line.
func (r *runtimeServices) networkError(err error) error {
	if api.Retryable(err) {
		return &ExitError{Code: 1, Message: fmt.Sprintf("Can't reach %s. Check your connection, then try again.", r.apiHostLabel())}
	}
	if api.Status(err) == 401 {
		return &ExitError{Code: 1, Message: "Your sign-in has expired. Run recallstack login."}
	}
	if code := api.Code(err); strings.HasPrefix(code, "TOTP_") || code == "LEGAL_CONSENT_REQUIRED" {
		return &ExitError{Code: 1, Message: "Finish signing in on the website, then try again."}
	}
	if message := api.Message(err); message != "" {
		return &ExitError{Code: 1, Message: message}
	}
	return err
}
