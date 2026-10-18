package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/o-clan/recallstack-cli/internal/api"
	"github.com/o-clan/recallstack-cli/internal/config"
	"github.com/o-clan/recallstack-cli/internal/integrations"
	"github.com/o-clan/recallstack-cli/internal/state"
)

func (r *runtimeServices) runAuth(args []string) error {
	switch args[0] {
	case "login":
		return r.runLogin(args[1:])
	case "logout":
		if len(args) != 1 {
			return fmt.Errorf("logout does not accept arguments")
		}
		return r.runLogout()
	case "whoami":
		parsed, err := parseOptions(args[1:], newOptionSpec(nil, []string{"json"}))
		if err != nil {
			return err
		}
		return r.runWhoami(parsed.bools["json"])
	}
	return fmt.Errorf("unknown auth command %q", args[0])
}

type deviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type deviceToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	User         struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		FirstName string `json:"first_name"`
	} `json:"user"`
	DeviceName string `json:"device_name"`
}

func deviceName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "this computer"
	}
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// apiHostLabel is what the "Can't reach …" line names.
func (r *runtimeServices) apiHostLabel() string {
	base := r.svc.BaseURL()
	if base == config.DefaultBaseURL {
		return "recallstack.com"
	}
	if parsed, err := url.Parse(base); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return base
}

func (r *runtimeServices) runLogin(args []string) error {
	parsed, err := parseOptions(args, newOptionSpec(nil, []string{"no-browser"}))
	if err != nil {
		return err
	}
	if len(parsed.positionals) > 0 {
		return fmt.Errorf("recallstack login no longer takes a code. Run recallstack login and approve in the browser")
	}
	out := r.deps.Stdout
	ctx := context.Background()
	cfg, err := r.svc.Config()
	if err != nil {
		return err
	}
	if cfg.AccessToken != "" || cfg.RefreshToken != "" || cfg.APIKey != "" {
		email := state.LoadMe().Email
		if email == "" {
			meCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			me, meErr := r.svc.Me(meCtx)
			cancel()
			if meErr == nil {
				email = me.Email
			}
		}
		if email == "" {
			email = "your account"
		}
		fmt.Fprintf(out, "Already signed in as %s. To switch accounts, run recallstack logout first.\n", email)
		return nil
	}

	device := deviceName()
	client := r.svc.Auth.Client(r.svc.BaseURL())
	var start deviceStart
	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = client.DoJSON(startCtx, "/v1/auth/cli/device", api.RequestOptions{Method: http.MethodPost, Body: map[string]any{
		"client": "recallstack-cli", "cli_version": r.deps.Version, "device_name": device, "os": runtime.GOOS,
	}}, &start)
	cancel()
	if err != nil {
		if api.Retryable(err) {
			return &ExitError{Code: 1, Message: fmt.Sprintf("✗ Can't reach %s. Check your connection, then run recallstack login again.", r.apiHostLabel())}
		}
		return err
	}
	if start.Interval <= 0 {
		start.Interval = 2
	}
	if start.ExpiresIn <= 0 {
		start.ExpiresIn = 600
	}
	display := strings.TrimPrefix(strings.TrimPrefix(start.VerificationURI, "https://"), "http://")
	openTarget := start.VerificationURIComplete
	if openTarget == "" {
		openTarget = start.VerificationURI
	}
	opened := false
	if !parsed.bools["no-browser"] && canOpenBrowser() {
		opened = openBrowser(openTarget) == nil
	}
	if opened {
		fmt.Fprintf(out, "Opening %s in your browser.\n\n", display)
		fmt.Fprintf(out, "    %s\n\n", start.UserCode)
		fmt.Fprintln(out, "Check that the browser shows this code, then approve.")
		fmt.Fprintf(out, "No browser here? Open %s on any device and enter the code.\n\n", start.VerificationURI)
	} else {
		fmt.Fprintf(out, "No browser here? Open %s on any device and enter the code.\n\n", start.VerificationURI)
		fmt.Fprintf(out, "    %s\n\n", start.UserCode)
		fmt.Fprintln(out, "Check that the browser shows this code, then approve.")
		fmt.Fprintln(out)
	}
	minutes := (start.ExpiresIn + 59) / 60
	fmt.Fprintf(out, "Waiting for approval  ·  expires in %d min  ·  Ctrl+C to cancel\n", minutes)

	token, err := r.pollDevice(ctx, client, start)
	if err != nil {
		return err
	}
	if token.DeviceName != "" {
		device = token.DeviceName
	}
	if err := r.svc.Auth.SaveTokens(r.svc.BaseURL(), token.AccessToken, token.RefreshToken); err != nil {
		return err
	}
	cfg, _ = r.svc.Reload()
	_ = state.SaveMe(state.Me{UserID: token.User.ID, Email: token.User.Email, FirstName: token.User.FirstName, DeviceName: device})
	meCtx, meCancel := context.WithTimeout(ctx, 3*time.Second)
	_, _ = r.svc.Me(meCtx)
	meCancel()

	flushCtx, flushCancel := context.WithTimeout(ctx, 20*time.Second)
	replay, _ := r.svc.Flush(flushCtx)
	flushCancel()

	fmt.Fprintln(out)
	fmt.Fprintf(out, "✓ Signed in as %s on %s\n", token.User.Email, device)
	if cfg.TokenStorage == "keychain" {
		fmt.Fprintln(out, "Sign-in saved to your keyring.")
	} else {
		fmt.Fprintf(out, "Sign-in saved to %s.\n", homeShort(state.ConfigPath()))
	}
	if replay.Replayed > 0 {
		noun := "records that were waiting have"
		if replay.Replayed == 1 {
			noun = "record that was waiting has"
		}
		fmt.Fprintf(out, "%d %s been sent.\n", replay.Replayed, noun)
	}
	if !r.anyAgentConnected() {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Next, in a repo you work in:  recallstack agent install")
	}
	return nil
}

func (r *runtimeServices) pollDevice(ctx context.Context, client *api.Client, start deviceStart) (deviceToken, error) {
	interval := time.Duration(start.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(start.ExpiresIn)*time.Second + 5*time.Second)
	transportFailures := 0
	for {
		if time.Now().After(deadline) {
			return deviceToken{}, &ExitError{Code: 1, Message: "✗ The code expired before it was approved.\n  Run recallstack login for a new one."}
		}
		time.Sleep(interval)
		var token deviceToken
		pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := client.DoJSON(pollCtx, "/v1/auth/cli/device/token", api.RequestOptions{Method: http.MethodPost, Body: map[string]any{"device_code": start.DeviceCode}}, &token)
		cancel()
		if err == nil && token.AccessToken != "" {
			return token, nil
		}
		switch api.Code(err) {
		case "authorization_pending":
			transportFailures = 0
			continue
		case "slow_down":
			interval += 2 * time.Second
			continue
		case "access_denied":
			return deviceToken{}, &ExitError{Code: 1, Message: "✗ Sign-in was cancelled in the browser. Nothing was saved.\n  Run recallstack login to try again."}
		case "expired_token":
			return deviceToken{}, &ExitError{Code: 1, Message: "✗ The code expired before it was approved.\n  Run recallstack login for a new one."}
		}
		if api.Retryable(err) {
			transportFailures++
			if transportFailures >= 5 {
				return deviceToken{}, &ExitError{Code: 1, Message: fmt.Sprintf("✗ Can't reach %s. Check your connection, then run recallstack login again.", r.apiHostLabel())}
			}
			continue
		}
		if err == nil {
			err = fmt.Errorf("the server returned no sign-in token")
		}
		return deviceToken{}, err
	}
}

func canOpenBrowser() bool {
	if strings.TrimSpace(os.Getenv("RECALLSTACK_BROWSER")) != "" {
		return true
	}
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd", "netbsd":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		_, err := exec.LookPath("xdg-open")
		return err == nil
	}
	return true
}

func openBrowser(target string) error {
	if command := strings.TrimSpace(os.Getenv("RECALLSTACK_BROWSER")); command != "" {
		return exec.Command(command, target).Start()
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (r *runtimeServices) anyAgentConnected() bool {
	manager, err := integrations.New(r.cwd)
	if err != nil {
		return false
	}
	for _, result := range manager.Detect(integrations.ScopeGlobal) {
		if result.Connected {
			return true
		}
	}
	return false
}

func (r *runtimeServices) runLogout() error {
	cfg, err := r.svc.Config()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.svc.Auth.Logout(ctx, &cfg)
	if err != nil {
		return err
	}
	me := state.LoadMe()
	_ = state.SaveMe(state.Me{DeviceName: me.DeviceName})
	if !result.RemoteRevoked && cfg.AccessToken != "" {
		fmt.Fprintln(r.deps.Stdout, "Signed out on this computer. The server couldn't be told; the sign-in will expire on its own.")
		return nil
	}
	fmt.Fprintln(r.deps.Stdout, "Signed out on this computer.")
	return nil
}

func (r *runtimeServices) runWhoami(asJSON bool) error {
	if !r.svc.SignedIn() {
		return &ExitError{Code: 1, Message: "Not signed in. Run recallstack login."}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	me, err := r.svc.Me(ctx)
	if err != nil && me.Email == "" {
		return err
	}
	device := me.DeviceName
	if device == "" {
		device = deviceName()
	}
	if asJSON {
		return writeJSON(r.deps.Stdout, map[string]any{"user_id": me.UserID, "email": me.Email, "first_name": me.FirstName, "tz": me.TZ, "device_name": device, "api": r.svc.BaseURL(), "offline": err != nil})
	}
	fmt.Fprintf(r.deps.Stdout, "Signed in as %s on %s\n", me.Email, device)
	return nil
}

func (r *runtimeServices) runConfig(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "where":
		_, err := fmt.Fprintln(r.deps.Stdout, state.ConfigPath())
		return err
	case "show":
		cfg, err := r.svc.Config()
		if err != nil {
			return err
		}
		return writeJSON(r.deps.Stdout, map[string]any{
			"config_path":        state.ConfigPath(),
			"state_root":         state.Root(),
			"base_url_override":  nullable(cfg.BaseURL),
			"default_base_url":   config.DefaultBaseURL,
			"effective_base_url": cfg.EffectiveBaseURL,
			"auth_status":        r.authStatus(),
		})
	case "base-url":
		parsed, err := parseOptions(args[1:], newOptionSpec(nil, []string{"clear"}))
		if err != nil {
			return err
		}
		if len(parsed.positionals) > 1 {
			return fmt.Errorf("config base-url accepts at most one URL")
		}
		value := ""
		if len(parsed.positionals) == 1 {
			value = parsed.positionals[0]
		}
		if parsed.bools["clear"] == (value != "") {
			if value == "" {
				cfg, _ := r.svc.Config()
				fmt.Fprintln(r.deps.Stdout, cfg.EffectiveBaseURL)
				return nil
			}
			return fmt.Errorf("provide either a URL or --clear, not both")
		}
		if err := r.svc.Store.SaveBaseURLOverride(r.cwd, value); err != nil {
			return err
		}
		cfg, err := r.svc.Reload()
		if err != nil {
			return err
		}
		fmt.Fprintf(r.deps.Stdout, "API is now %s\n", cfg.EffectiveBaseURL)
		return nil
	}
	return fmt.Errorf("unknown config command %q", args[0])
}

func (r *runtimeServices) authStatus() map[string]any {
	cfg, _ := r.svc.Config()
	queue := r.svc.Queue().List(cfg.EffectiveBaseURL)
	authBlocked := 0
	for _, record := range queue {
		if record.QueueReason == "auth_required" {
			authBlocked++
		}
	}
	authenticated := cfg.AccessToken != "" || cfg.RefreshToken != "" || cfg.APIKey != ""
	status := "login_required"
	if authenticated {
		status = "authenticated"
	}
	if authBlocked > 0 {
		if authenticated {
			status = "authenticated_with_pending_auth_replay"
		} else {
			status = "login_required_with_pending_replay"
		}
	}
	storage := cfg.TokenStorage
	if storage == "" {
		storage = "file"
	}
	return map[string]any{
		"authenticated": authenticated, "token_storage": storage, "status": status,
		"pending_writes": len(queue), "auth_blocked_pending_writes": authBlocked, "login_required": !authenticated || authBlocked > 0,
	}
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func choose[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}

// homeShort prints a path with ~ for the home folder.
func homeShort(path string) string {
	home := state.Home()
	if home != "" && strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}
