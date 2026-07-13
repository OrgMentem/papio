// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStructuralTimeoutKillsWorkerProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not provide POSIX process groups")
	}
	marker := filepath.Join(t.TempDir(), "orphan-ran")
	worker := fakeTool(t, fmt.Sprintf(`cat >/dev/null; (sleep 0.2; : > %q) & sleep 10`, marker))

	_, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{Timeout: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected timed-out worker")
	}
	time.Sleep(300 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worker child survived timeout: stat error %v", statErr)
	}
}
