// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package store

import (
	"path/filepath"
	"strings"
)

// uriPathEscaper escapes the only bytes SQLite's URI parser treats specially in
// the path part of a "file:" URI: '%' starts a percent-escape, '?' starts the
// query and '#' starts the fragment.
var uriPathEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// FileDSN returns a modernc.org/sqlite DSN for the database file at path.
// With the "file:" prefix SQLite applies its URI rules to the path, so a data
// directory containing '#', '?' or '%' would otherwise open or create the wrong
// file and drop the pragmas. query is the already-encoded query string without
// the leading '?' (for example "mode=ro&_pragma=busy_timeout(5000)"); it may be
// empty.
func FileDSN(path, query string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' && filepath.VolumeName(path) != "" {
		// SQLite expects file:///C:/dir/db.sqlite for a Windows drive path.
		p = "///" + p
	}
	dsn := "file:" + uriPathEscaper.Replace(p)
	if query != "" {
		dsn += "?" + query
	}
	return dsn
}
