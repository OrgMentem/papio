// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package artifact

// reassertReadOnly restores the read-only attribute that removing the linked
// quarantine name cleared. On Windows the attribute belongs to the file, not to
// a name, and os.Remove clears it before it deletes a read-only name, so the
// published artifact became writable. Best-effort like the cleanup it follows:
// the artifact is published either way, and Verify still checks its bytes.
func reassertReadOnly(path string) { _ = chmodFile(path, 0o400) }
