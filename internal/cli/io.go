package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func readAll(reader io.Reader) (string, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func readJSONObject(raw, label string) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("invalid %s JSON: %w", label, err)
	}
	if value == nil {
		return nil, fmt.Errorf("invalid %s JSON: value must be an object", label)
	}
	return value, nil
}

func promptLine(input io.Reader, output io.Writer, prompt string) (string, error) {
	if file, ok := input.(*os.File); ok {
		info, err := file.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice == 0 {
			return "", fmt.Errorf("interactive input requires a TTY")
		}
	}
	if file, ok := output.(*os.File); ok {
		info, err := file.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice == 0 {
			return "", fmt.Errorf("interactive output requires a TTY")
		}
	}
	if _, err := io.WriteString(output, prompt); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
