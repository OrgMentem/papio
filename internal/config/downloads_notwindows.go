// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package config

// windowsDownloadsDir is the non-Windows stub for the FOLDERID_Downloads
// registry lookup; platformDownloadsDir never calls it off Windows.
func windowsDownloadsDir() string { return "" }
