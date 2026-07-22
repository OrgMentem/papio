package institution

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var zoteroResolverPref = regexp.MustCompile(`^\s*user_pref\s*\(\s*["']extensions\.zotero\.openURL\.resolver["']\s*,\s*["']([^"']*)["']\s*\)\s*;?\s*$`)

// ZoteroResolver returns the first HTTPS OpenURL resolver configured in a
// default Zotero profile. It is best-effort: missing or malformed profiles are
// ignored.
func ZoteroResolver() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}

	roots := []string{filepath.Join(home, "Zotero")}
	switch runtime.GOOS {
	case "darwin":
		roots = append(roots, filepath.Join(home, "Library", "Application Support", "Zotero"))
	case "linux":
		roots = append(roots, filepath.Join(home, ".zotero", "zotero"))
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			roots = append(roots, filepath.Join(appData, "Zotero", "Zotero"))
		}
	}
	return zoteroResolverIn(roots)
}

func zoteroResolverIn(roots []string) (string, bool) {
	for _, root := range roots {
		paths, _ := filepath.Glob(filepath.Join(root, "Profiles", "*", "prefs.js"))
		paths = append(paths, filepath.Join(root, "prefs.js"))
		for _, path := range paths {
			resolver, ok := zoteroResolverFile(path)
			if ok {
				return resolver, true
			}
		}
	}
	return "", false
}

func zoteroResolverFile(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		match := zoteroResolverPref.FindStringSubmatch(scanner.Text())
		if len(match) != 2 {
			continue
		}
		resolver := unescapeZoteroPref(match[1])
		if strings.HasPrefix(resolver, "https://") {
			return resolver, true
		}
	}
	return "", false
}

func unescapeZoteroPref(value string) string {
	var decoded strings.Builder
	decoded.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			switch value[i+1] {
			case '/', '\\':
				decoded.WriteByte(value[i+1])
				i++
				continue
			}
		}
		decoded.WriteByte(value[i])
	}
	return decoded.String()
}
