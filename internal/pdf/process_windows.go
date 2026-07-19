// Copyright 2026 OrgMentem. Licensed under MIT.
//go:build windows

package pdf

import "os/exec"

// configureProcessTree is a deliberate no-op on Windows. The Unix build places
// the converter in its own process group so a timeout can SIGKILL the whole
// tree; Windows has no equivalent process-group semantics. exec.CommandContext's
// default cancel already calls TerminateProcess on the direct child, and papio
// launches every external tool (pdftoppm, pdftotext, tesseract, and the papio
// PDF worker) directly with no intervening shell, so a cancelled conversion
// leaves no orphaned grandchildren to reap. Assigning the child to a Job Object
// with KILL_ON_JOB_CLOSE would be the equivalent hardening, but it must happen
// before the child spawns any descendants (CREATE_SUSPENDED, assign, resume),
// which this call site cannot arrange; it is only worth adding if a future tool
// spawns its own children.
func configureProcessTree(cmd *exec.Cmd) {}
