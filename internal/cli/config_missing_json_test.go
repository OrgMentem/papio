package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunMissingExplicitConfigIsInvalidArgumentUnderJSON: an explicit --config
// that names no file is a mistyped argument, so --json reports invalid_argument
// (not the generic `error` code a script would retry or escalate), and the
// message that tells the operator how to create the file is unchanged.
func TestRunMissingExplicitConfigIsInvalidArgumentUnderJSON(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"--json", "--config", missing, "jobs", "list"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	got := decodeJSONError(t, errOut.String())
	if got["code"] != "invalid_argument" {
		t.Fatalf("error = %v, want code invalid_argument", got)
	}
	message, _ := got["message"].(string)
	if !strings.Contains(message, "config file "+missing+" does not exist") || !strings.Contains(message, "papio init --config "+missing) {
		t.Fatalf("message = %q, want it to name the path and how to create it", message)
	}
}
