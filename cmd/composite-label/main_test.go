package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestLaunchReapsExitedChild pins that opening a file does not leave a zombie
// per keypress: the child launched without blocking the review loop must still
// be waited on, which is what records its ProcessState.
func TestLaunchReapsExitedChild(t *testing.T) {
	if os.Getenv("COMPOSITE_LABEL_HELPER") == "1" {
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLaunchReapsExitedChild$")
	cmd.Env = append(os.Environ(), "COMPOSITE_LABEL_HELPER=1")
	done, err := launch(cmd)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("exited child was never reaped")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("ProcessState = %v, want a reaped exit", cmd.ProcessState)
	}
}
