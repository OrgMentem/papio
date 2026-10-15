package daemon

import (
	"strings"
	"testing"
)

// A daemon blocked on a keychain read must not be killed as a hung start.
func TestCredentialsPhaseIsStartupWork(t *testing.T) {
	status := Status{Phase: PhaseCredentials}
	if !status.Phase.storeWork() {
		t.Fatal("credentials phase is not exempt from the start deadline")
	}
	if got := status.Activity(); !strings.Contains(got, "keychain") {
		t.Fatalf("Activity() = %q, want it to name the keychain", got)
	}
	if phaseOrder[PhaseStarting] >= phaseOrder[PhaseCredentials] || phaseOrder[PhaseCredentials] >= phaseOrder[PhaseUpgrading] {
		t.Fatal("credentials phase must sit between starting and upgrading")
	}
}
