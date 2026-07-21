package cli

import "testing"

func TestParseOptionsAllowsFlagsAfterPositionals(t *testing.T) {
	parsed, err := parseOptions(
		[]string{"codex", "gpt-5.4", "--global", "--cwd", "/tmp/work"},
		newOptionSpec([]string{"cwd"}, []string{"global", "clear"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.positionals) != 2 || parsed.positionals[0] != "codex" || parsed.positionals[1] != "gpt-5.4" {
		t.Fatalf("unexpected positionals: %#v", parsed.positionals)
	}
	if !parsed.bools["global"] || parsed.values["cwd"] != "/tmp/work" {
		t.Fatalf("unexpected options: %#v %#v", parsed.bools, parsed.values)
	}
}

func TestParseOptionsSupportsEqualsAndTerminator(t *testing.T) {
	parsed, err := parseOptions(
		[]string{"--query=what changed", "--", "--literal"},
		newOptionSpec([]string{"query"}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.values["query"] != "what changed" || len(parsed.positionals) != 1 || parsed.positionals[0] != "--literal" {
		t.Fatalf("unexpected parse result: %#v", parsed)
	}
}

func TestParseOptionsRejectsUnknownAndMissingValues(t *testing.T) {
	if _, err := parseOptions([]string{"--unknown"}, newOptionSpec(nil, nil)); err == nil {
		t.Fatal("expected unknown option error")
	}
	if _, err := parseOptions([]string{"--cwd"}, newOptionSpec([]string{"cwd"}, nil)); err == nil {
		t.Fatal("expected missing value error")
	}
}
