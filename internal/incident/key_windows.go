// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package incident

import "os"

// Windows Chmod only changes the read-only attribute, not file privacy. New
// temporary files inherit the data directory's ACL, and exclusive hard-link
// publication retains that descriptor. Existing keys retain their descriptor
// and attributes. Data-directory privacy must be established by its Windows
// ACL (as diagnosed by doctor), not by this function's Unix permission bits.
func restrictIncidentKey(_ *os.File) error { return nil }

// The key's data was flushed through its writable file handle before closing
// and exclusive publication. Go's directory handle is not a writable file
// handle suitable for FlushFileBuffers: Sync reports ERROR_ACCESS_DENIED.
// There is no Unix-style directory sync here, and no promise that the new
// directory entry survives a power loss. Do not swallow errors from the actual
// temporary-file Sync or substitute a privileged volume-wide flush.
func syncIncidentKeyDirectory(_ string) error { return nil }
