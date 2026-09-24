// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package artifact

// reassertReadOnly has nothing to do: a POSIX mode belongs to the inode, and
// removing the quarantine name leaves the published artifact's 0400 as it was.
func reassertReadOnly(string) {}
