// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package agentcredential

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// Profile identifies a configuration and its data directory without exposing
// either path to the OS credential store. Paths need not exist. Windows spelling
// is case-insensitive; symlinks are not resolved on any platform.
func Profile(configPath, dataDir string) (string, error) {
	paths := [2]string{configPath, dataDir}
	for i, path := range paths {
		if path == "" || strings.IndexByte(path, 0) >= 0 || !utf8.ValidString(path) {
			return "", ErrInvalidProfile
		}
		absolute, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return "", ErrInvalidProfile
		}
		if runtime.GOOS == "windows" {
			absolute = strings.ToLower(absolute)
		}
		paths[i] = absolute
	}
	// A JSON pair is length-unambiguous, including paths with embedded separators.
	encoded, err := json.Marshal(paths)
	if err != nil {
		return "", ErrInvalidProfile
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validProfile(profile string) bool {
	if len(profile) != 64 {
		return false
	}
	for i := range len(profile) {
		c := profile[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
