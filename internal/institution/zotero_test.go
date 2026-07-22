package institution

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZoteroResolverInFindsProfilePref(t *testing.T) {
	root := t.TempDir()
	writeZoteroPrefs(t, filepath.Join(root, "Profiles", "abc.default", "prefs.js"), `
user_pref("browser.startup.homepage", "https://example.test");
user_pref("extensions.zotero.openURL.resolver", "https://resolver.example.edu/openurl");
`)

	got, ok := zoteroResolverIn([]string{root})
	if !ok || got != "https://resolver.example.edu/openurl" {
		t.Fatalf("zoteroResolverIn() = %q, %t; want resolver, true", got, ok)
	}
}

func TestZoteroResolverInAcceptsSingleQuotedPref(t *testing.T) {
	root := t.TempDir()
	writeZoteroPrefs(t, filepath.Join(root, "prefs.js"), `user_pref( 'extensions.zotero.openURL.resolver' , 'https://resolver.example.edu/openurl' );`)

	got, ok := zoteroResolverIn([]string{root})
	if !ok || got != "https://resolver.example.edu/openurl" {
		t.Fatalf("zoteroResolverIn() = %q, %t; want resolver, true", got, ok)
	}
}

func TestZoteroResolverInSkipsHTTPResolver(t *testing.T) {
	root := t.TempDir()
	writeZoteroPrefs(t, filepath.Join(root, "prefs.js"), `user_pref("extensions.zotero.openURL.resolver", "http://resolver.example.edu/openurl");`)

	if got, ok := zoteroResolverIn([]string{root}); ok || got != "" {
		t.Fatalf("zoteroResolverIn() = %q, %t; want empty, false", got, ok)
	}
}

func TestZoteroResolverInReturnsFalseWithoutPref(t *testing.T) {
	root := t.TempDir()
	writeZoteroPrefs(t, filepath.Join(root, "prefs.js"), `user_pref("browser.startup.homepage", "https://example.test");`)

	if got, ok := zoteroResolverIn([]string{root}); ok || got != "" {
		t.Fatalf("zoteroResolverIn() = %q, %t; want empty, false", got, ok)
	}
}

func TestZoteroResolverInFindsLaterRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeZoteroPrefs(t, filepath.Join(first, "prefs.js"), `user_pref("browser.startup.homepage", "https://example.test");`)
	writeZoteroPrefs(t, filepath.Join(second, "prefs.js"), `user_pref("extensions.zotero.openURL.resolver", "https://resolver.example.edu/openurl");`)

	got, ok := zoteroResolverIn([]string{first, second})
	if !ok || got != "https://resolver.example.edu/openurl" {
		t.Fatalf("zoteroResolverIn() = %q, %t; want resolver from later root, true", got, ok)
	}
}

func TestZoteroResolverInDecodesEscapedSlashes(t *testing.T) {
	root := t.TempDir()
	writeZoteroPrefs(t, filepath.Join(root, "prefs.js"), `user_pref("extensions.zotero.openURL.resolver", "https:\/\/resolver.example.edu\/openurl");`)

	got, ok := zoteroResolverIn([]string{root})
	if !ok || got != "https://resolver.example.edu/openurl" {
		t.Fatalf("zoteroResolverIn() = %q, %t; want decoded resolver, true", got, ok)
	}
}

func writeZoteroPrefs(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
