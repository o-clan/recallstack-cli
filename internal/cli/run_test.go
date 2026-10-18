package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpNeverDispatchesMutatingCommand(t *testing.T) {
	for _, args := range [][]string{
		{"project", "clear", "--help"},
		{"config", "base-url", "--clear", "--help"},
		{"agent", "uninstall", "codex", "--help"},
	} {
		var stdout bytes.Buffer
		err := Run(args, Dependencies{Version: "1.2.0", Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &bytes.Buffer{}})
		if err != nil {
			t.Fatalf("Run(%q): %v", args, err)
		}
		if !strings.HasPrefix(stdout.String(), "Usage:\n") {
			t.Fatalf("Run(%q) returned unexpected help: %q", args, stdout.String())
		}
	}
}
