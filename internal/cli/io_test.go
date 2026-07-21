package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteJSONUsesPrettyStableShape(t *testing.T) {
	var output bytes.Buffer
	if err := writeJSON(&output, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "{\n  \"ok\": true\n}\n" {
		t.Fatalf("unexpected JSON: %q", output.String())
	}
}

func TestReadJSONObject(t *testing.T) {
	value, err := readJSONObject(`{"name":"recallstack"}`, "metadata")
	if err != nil || value["name"] != "recallstack" {
		t.Fatalf("unexpected result: %#v %v", value, err)
	}
	if _, err := readJSONObject(`[]`, "metadata"); err == nil {
		t.Fatal("expected array rejection")
	}
}

func TestPromptLine(t *testing.T) {
	var output bytes.Buffer
	value, err := promptLine(strings.NewReader(" value \n"), &output, "Code: ")
	if err != nil || value != "value" || output.String() != "Code: " {
		t.Fatalf("unexpected prompt result: %q %q %v", value, output.String(), err)
	}
}
