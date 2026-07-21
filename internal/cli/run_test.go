package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersionAndHelp(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-V"}} {
		var stdout bytes.Buffer
		err := Run(args, Dependencies{Version: "1.2.0", Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &bytes.Buffer{}})
		if err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "1.2.0\n" {
			t.Fatalf("unexpected version output: %q", stdout.String())
		}
	}

	var stdout bytes.Buffer
	if err := Run(nil, Dependencies{Version: "1.2.0", Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Recallstack CLI") || !strings.Contains(stdout.String(), "memory") {
		t.Fatalf("unexpected help output: %q", stdout.String())
	}
}

func TestRewriteAliases(t *testing.T) {
	tests := []struct {
		input []string
		want  string
	}{
		{[]string{"target", "where"}, "project where"},
		{[]string{"config"}, "config show"},
		{[]string{"workspace", "--global"}, "workspace show --global"},
		{[]string{"project", "--help"}, "project --help"},
	}
	for _, test := range tests {
		if got := strings.Join(rewriteAliases(test.input), " "); got != test.want {
			t.Fatalf("rewriteAliases(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

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
