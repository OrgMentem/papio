// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !darwin && !linux && !windows

package ownershipsnapshot

import "os"

func openSource(path string) (*os.File, error) {
	return os.Open(path)
}

// Unknown metadata prevents revision reuse. snapshot.go therefore verifies a
// freshly read descriptor against a bounded reread of the current pathname.
func fileIdentity(_ *os.File, _ os.FileInfo) fileID {
	return fileID{}
}
