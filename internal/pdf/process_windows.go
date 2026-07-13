// Copyright 2026 OrgMentem. Licensed under MIT.
//go:build windows

package pdf

import "os/exec"

// CommandContext still terminates the direct child on Windows.
func configureProcessTree(cmd *exec.Cmd) {}
