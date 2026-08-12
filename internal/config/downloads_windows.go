// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package config

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// downloadsKnownFolderID is FOLDERID_Downloads. Unlike Desktop or Documents,
// Downloads has no legacy named value under "Shell Folders", so the GUID form
// under "User Shell Folders" is the only registry route to a relocated
// Downloads folder.
const downloadsKnownFolderID = `{374DE290-123F-4565-9164-39C4925E467B}`

// windowsDownloadsDir reads the current user's Downloads known folder. Every
// failure returns "" so UserDownloadsDir falls back to %USERPROFILE%\Downloads,
// which is where the known folder points on an untouched profile anyway.
func windowsDownloadsDir() string {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`,
		registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer func() { _ = key.Close() }()

	value, _, err := key.GetStringValue(downloadsKnownFolderID)
	if err != nil {
		return ""
	}
	// The value is normally REG_EXPAND_SZ holding %USERPROFILE%\Downloads.
	expanded, err := registry.ExpandString(value)
	if err != nil || strings.TrimSpace(expanded) == "" {
		return ""
	}
	return filepath.Clean(expanded)
}
