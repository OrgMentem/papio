// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package agentcredential

import "os/exec"

// configureHelperProcess is a no-op on Windows: the default cancellation
// terminates the helper, and Credential Manager is called through the Win32 API
// in that process, not through a child program.
func configureHelperProcess(*exec.Cmd) {}
