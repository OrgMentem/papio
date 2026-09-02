package institution

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZoteroResolverIn(t *testing.T) {
	const (
		homepageOnly = `user_pref("browser.startup.homepage", "https://example.test");`
		resolverPref = `user_pref("extensions.zotero.openURL.resolver", "https://resolver.example.edu/openurl");`
		wantResolver = "https://resolver.example.edu/openurl"
	)
	for _, test := range []struct {
		name      string
		prefs     []string // one prefs.js body per root, in lookup order
		pathShape string   // root-relative location of every prefs.js
		wantURL   string
		wantOK    bool
	}{
		{name: "finds profile pref", prefs: []string{homepageOnly + "\n" + resolverPref}, pathShape: filepath.Join("Profiles", "abc.default", "prefs.js"), wantURL: wantResolver, wantOK: true},
		{name: "accepts single quoted pref", prefs: []string{`user_pref( 'extensions.zotero.openURL.resolver' , 'https://resolver.example.edu/openurl' );`}, pathShape: "prefs.js", wantURL: wantResolver, wantOK: true},
		{name: "skips http resolver", prefs: []string{`user_pref("extensions.zotero.openURL.resolver", "http://resolver.example.edu/openurl");`}, pathShape: "prefs.js", wantURL: "", wantOK: false},
		{name: "returns false without pref", prefs: []string{homepageOnly}, pathShape: "prefs.js", wantURL: "", wantOK: false},
		{name: "finds resolver in a later root", prefs: []string{homepageOnly, resolverPref}, pathShape: "prefs.js", wantURL: wantResolver, wantOK: true},
		{name: "decodes escaped slashes", prefs: []string{`user_pref("extensions.zotero.openURL.resolver", "https:\/\/resolver.example.edu\/openurl");`}, pathShape: "prefs.js", wantURL: wantResolver, wantOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := make([]string, 0, len(test.prefs))
			for _, body := range test.prefs {
				root := t.TempDir()
				writeZoteroPrefs(t, filepath.Join(root, test.pathShape), body)
				roots = append(roots, root)
			}
			got, ok := zoteroResolverIn(roots)
			if got != test.wantURL || ok != test.wantOK {
				t.Fatalf("zoteroResolverIn(%d roots) = %q, %t; want %q, %t", len(roots), got, ok, test.wantURL, test.wantOK)
			}
		})
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
