// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// shimDir holds the one dispatcher script every fakeTool links to. It outlives
// any single test, so TestMain removes it.
var shimDir struct {
	once sync.Once
	dir  string
	path string
	err  error
}

// fakeToolShim returns the dispatcher: a script that runs the body stored next
// to the path it was invoked by. exec replaces the dispatcher with /bin/sh, so
// the tool keeps the dispatcher's pid and process group, as a standalone
// script would.
func fakeToolShim(t *testing.T) string {
	t.Helper()
	shimDir.once.Do(func() {
		shimDir.dir, shimDir.err = os.MkdirTemp("", "papio-pdf-faketool-")
		if shimDir.err != nil {
			return
		}
		shimDir.path = filepath.Join(shimDir.dir, "shim")
		shimDir.err = os.WriteFile(shimDir.path, []byte("#!/bin/sh\nexec /bin/sh \"$0.body\" \"$@\"\n"), 0o700)
	})
	if shimDir.err != nil {
		t.Fatalf("fake tool dispatcher: %v", shimDir.err)
	}
	return shimDir.path
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shimDir.dir != "" {
		_ = os.RemoveAll(shimDir.dir)
	}
	os.Exit(code)
}
