// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AdoptionDirName is the one path segment a browser-steered download can be
// redirected into, and it is not a preference: Chrome's
// onDeterminingFilename may only rewrite a download to a path RELATIVE to the
// browser's own download directory, and every steering target the extension
// mints hardcodes this segment — "papio/<job-id>/<name>" for job downloads,
// and "papio/grabs/<grab-id>/" for PDF grabs, which
// protocol.PDFGrabResult freezes on the wire. A daemon adoption root that is
// not exactly <browser download dir>/papio can therefore never receive a
// steered download, however readable it is.
const AdoptionDirName = "papio"

// legacyAdoptionDirName is the pre-2026-08 default adoption root's name under
// the data directory. It was never reachable by browser steering (see
// AdoptionDirName), so nothing was ever adopted from a default install; it is
// retained only as a drain-only search path so installs that were configured
// by hand — or that received files some other way — do not lose them.
const legacyAdoptionDirName = "adoptions"

// DefaultAdoptionRoot is the adoption root used when download_adoption_root
// is empty: the "papio" directory inside the user's download directory. It is
// derived on every call rather than frozen into the config file, so a user
// who moves their download directory is followed rather than silently
// stranded.
func DefaultAdoptionRoot() string {
	return filepath.Join(UserDownloadsDir(), AdoptionDirName)
}

// UserDownloadsDir returns the directory browsers download into by default.
//
// Per platform:
//   - macOS: ~/Downloads. NSDownloadsDirectory is not relocatable per user
//     without moving the folder itself, and Chrome/Firefox both seed their
//     download directory from it.
//   - Windows: the FOLDERID_Downloads known folder read from the current
//     user's shell-folder registry values, because Windows genuinely does let
//     a user move Downloads to another volume; %USERPROFILE%\Downloads is the
//     fallback when the lookup fails.
//   - Everything else (Linux, BSD): the XDG user-dirs contract —
//     $XDG_DOWNLOAD_DIR, then XDG_DOWNLOAD_DIR in user-dirs.dirs, then
//     ~/Downloads. Parsing user-dirs.dirs is what makes a localized desktop
//     (~/Téléchargements, ~/Descargas) resolve correctly instead of creating
//     a second English-named folder no browser writes to.
//
// It never fails: an undiscoverable home yields a relative "Downloads", which
// doctor then reports as unreachable rather than silently adopting nothing.
func UserDownloadsDir() string {
	if dir := platformDownloadsDir(); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "Downloads"
	}
	return filepath.Join(home, "Downloads")
}

// platformDownloadsDir returns the platform's authoritative download
// directory, or "" to fall back to <home>/Downloads.
func platformDownloadsDir() string {
	switch runtime.GOOS {
	case "windows":
		return windowsDownloadsDir()
	case "darwin":
		return "" // ~/Downloads; there is no per-user relocation to consult
	default:
		return xdgDownloadDir()
	}
}

// xdgDownloadDir implements the XDG user-directories lookup order.
func xdgDownloadDir() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_DOWNLOAD_DIR")); dir != "" && filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		if home == "" {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	content, err := os.ReadFile(filepath.Join(configHome, "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	return xdgDownloadDirFrom(string(content), home)
}

// xdgDownloadDirFrom extracts XDG_DOWNLOAD_DIR from a user-dirs.dirs body.
// The file is sourced by a shell, so a later assignment wins and values are
// either "$HOME/relative" or an absolute quoted path. A value of "$HOME"
// means the user deliberately disabled a separate download folder; that is
// not a directory papio should claim, so it resolves to "" and the caller
// falls back.
func xdgDownloadDirFrom(content, home string) string {
	found := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "XDG_DOWNLOAD_DIR" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			value = value[1 : len(value)-1]
		}
		switch {
		case strings.HasPrefix(value, "$HOME/"):
			rest := strings.Trim(strings.TrimPrefix(value, "$HOME/"), "/")
			if home == "" || rest == "" {
				found = ""
				continue
			}
			found = filepath.Join(home, filepath.FromSlash(rest))
		case filepath.IsAbs(value):
			found = filepath.Clean(value)
		default:
			found = ""
		}
	}
	return found
}

// BrowserSteerableAdoptionRoot reports whether a browser could ever steer a
// download into root. Steering supplies a relative path under the browser's
// download directory whose first segment is AdoptionDirName, so the root's
// final element must be exactly that segment. This is a structural fact about
// the download API, independent of whether the directory exists or is
// readable — a root that fails it adopts nothing, forever, silently.
func BrowserSteerableAdoptionRoot(root string) bool {
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}
	return filepath.Base(filepath.Clean(root)) == AdoptionDirName
}
