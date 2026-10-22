package cli

import (
	"strings"
	"testing"
	"time"
)

// TestParseSinceInstantAcceptsDays: `--since 7d` works on jobs failures, so
// export ledger and stats producers, which share this parser, must accept it too.
func TestParseSinceInstantAcceptsDays(t *testing.T) {
	before := time.Now()
	got, err := parseSinceInstant("7d")
	if err != nil {
		t.Fatalf("parseSinceInstant(7d): %v", err)
	}
	if want := before.Add(-7 * 24 * time.Hour); got.Before(want) || got.After(time.Now().Add(-7*24*time.Hour)) {
		t.Fatalf("parseSinceInstant(7d) = %s, want 7 days before now", got)
	}
	if _, err := parseSinceInstant("soon"); err == nil || !strings.Contains(err.Error(), `"soon"`) || !strings.Contains(err.Error(), "7d") {
		t.Fatalf("parseSinceInstant(soon) error = %v, want it to echo the value and name the forms", err)
	}
}
