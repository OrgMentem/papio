// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// Validate() on WorkRequest and AcquisitionBundle once measured text length in
// UTF-8 bytes and skipped several schema bounds entirely. The published schemas
// count Unicode code points (maxLength/minLength) and cap collection sizes, so
// the two layers disagreed in both directions: Go rejected valid multibyte text
// and accepted over-long collection names, source entries, evidence and notes.
//
// Each case below mutates a valid corpus fixture, then requires the published
// schema and the executable decoder to reach the SAME verdict. Cases sit in a
// table rather than one fixture file per bound: the boundary pairs (N ok, N+1
// rejected) are easier to audit side by side.

type boundsCase struct {
	name   string
	mutate func(doc map[string]any)
	valid  bool
}

// cjk is a three-byte code point; n of them are n runes but 3n bytes, which is
// what separates a rune count from a byte count.
func cjk(n int) string { return strings.Repeat("字", n) }

func stringList(n int, entry string) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = entry
	}
	return out
}

func runBoundsCases(t *testing.T, kind, fixture, schemaName string, decode func([]byte) error, cases []boundsCase) {
	t.Helper()
	schema := compileSchema(t, schemaName)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := readFixture(t, kind, fixture).(map[string]any)
			if !ok {
				t.Fatalf("fixture %s is not an object", fixture)
			}
			tc.mutate(base)
			raw, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			schemaErr := schema.Validate(doc)
			if (schemaErr == nil) != tc.valid {
				t.Fatalf("published schema verdict valid=%v, want %v (err: %v)", schemaErr == nil, tc.valid, schemaErr)
			}
			if decodeErr := decode(raw); (decodeErr == nil) != tc.valid {
				t.Fatalf("decoder verdict valid=%v, want %v (err: %v)", decodeErr == nil, tc.valid, decodeErr)
			}
		})
	}
}

func TestWorkRequestBoundsMatchPublishedSchema(t *testing.T) {
	decode := func(raw []byte) error { _, err := DecodeWorkRequest(raw); return err }
	set := func(key string, value any) func(map[string]any) {
		return func(doc map[string]any) { doc[key] = value }
	}
	runBoundsCases(t, "valid", "work-request-title-tuple.json", "work-request-v1.schema.json", decode, []boundsCase{
		{"title 500 multibyte runes", set("title", cjk(500)), true},
		{"title 501 multibyte runes", set("title", cjk(501)), false},
		{"title 3 runes of two bytes ok", set("title", "ééé"), true},
		{"title two multibyte runes below minimum", set("title", "éé"), false},
		{"title 500 ascii", set("title", strings.Repeat("a", 500)), true},
		{"title 501 ascii", set("title", strings.Repeat("a", 501)), false},
		{"author 200 multibyte runes", set("authors", []any{cjk(200)}), true},
		{"author 201 multibyte runes", set("authors", []any{cjk(201)}), false},
		{"author empty", set("authors", []any{""}), false},
		{"authors 100 entries", set("authors", stringList(100, "A")), true},
		{"authors 101 entries", set("authors", stringList(101, "A")), false},
		{"collection 200 multibyte runes", set("collection", cjk(200)), true},
		{"collection 201 multibyte runes", set("collection", cjk(201)), false},
		{"collection 201 ascii", set("collection", strings.Repeat("c", 201)), false},
		{"sources_allow entry 100 multibyte runes", set("sources_allow", []any{cjk(100)}), true},
		{"sources_allow entry 101 multibyte runes", set("sources_allow", []any{cjk(101)}), false},
		{"sources_deny entry 100 multibyte runes", set("sources_deny", []any{cjk(100)}), true},
		{"sources_deny entry 101 ascii", set("sources_deny", []any{strings.Repeat("s", 101)}), false},
		{"sources_allow 50 entries", set("sources_allow", stringList(50, "s")), true},
		{"sources_allow 51 entries", set("sources_allow", stringList(51, "s")), false},
		{"sources_deny 50 entries", set("sources_deny", stringList(50, "s")), true},
		{"sources_deny 51 entries", set("sources_deny", stringList(51, "s")), false},
	})
}

func TestAcquisitionBundleBoundsMatchPublishedSchema(t *testing.T) {
	decode := func(raw []byte) error { _, err := DecodeAcquisitionBundle(raw); return err }
	in := func(section, key string, value any) func(map[string]any) {
		return func(doc map[string]any) {
			switch section {
			case "":
				doc[key] = value
			default:
				var obj map[string]any
				obj, _ = doc[section].(map[string]any)
				obj[key] = value
			}
		}
	}
	// Attach bundles (zotio_item_key set) leave title and authors descriptive;
	// the full fixture is one, the min fixture is a new-item bundle. Cases run
	// against both so the bounds hold on either path.
	for _, fixture := range []string{"acquisition-bundle-min.json", "acquisition-bundle-full.json"} {
		t.Run(fixture, func(t *testing.T) {
			runBoundsCases(t, "valid", fixture, "acquisition-bundle-v1.schema.json", decode, []boundsCase{
				{"title 500 multibyte runes", in("identity", "title", cjk(500)), true},
				{"title 501 multibyte runes", in("identity", "title", cjk(501)), false},
				{"title two multibyte runes below minimum", in("identity", "title", "éé"), false},
				{"title 3 runes of two bytes ok", in("identity", "title", "ééé"), true},
				{"author 200 multibyte runes", in("identity", "authors", []any{cjk(200)}), true},
				{"author 201 multibyte runes", in("identity", "authors", []any{cjk(201)}), false},
				{"author empty", in("identity", "authors", []any{""}), false},
				{"authors 100 entries", in("identity", "authors", stringList(100, "A")), true},
				{"authors 101 entries", in("identity", "authors", stringList(101, "A")), false},
				{"evidence 20 entries of 300 multibyte runes", in("identity", "evidence", stringList(20, cjk(300))), true},
				{"evidence 21 entries", in("identity", "evidence", stringList(21, "e")), false},
				{"evidence entry 301 multibyte runes", in("identity", "evidence", []any{cjk(301)}), false},
				{"source 100 multibyte runes", in("candidate", "source", cjk(100)), true},
				{"source 101 multibyte runes", in("candidate", "source", cjk(101)), false},
				{"reuse_license 100 multibyte runes", in("candidate", "reuse_license", cjk(100)), true},
				{"reuse_license 101 multibyte runes", in("candidate", "reuse_license", cjk(101)), false},
				{"landing_url 2000 runes", in("candidate", "landing_url", "https://example.org/"+cjk(2000-len("https://example.org/"))), true},
				{"landing_url 2001 runes", in("candidate", "landing_url", "https://example.org/"+cjk(2001-len("https://example.org/"))), false},
				{"source_record_id 200 multibyte runes", in("candidate", "source_record_id", cjk(200)), true},
				{"source_record_id 201 multibyte runes", in("candidate", "source_record_id", cjk(201)), false},
				{"adapter_version 100 multibyte runes", in("", "adapter_version", cjk(100)), true},
				{"adapter_version 101 multibyte runes", in("", "adapter_version", cjk(101)), false},
				{"mime 100 ascii", in("artifact", "mime", strings.Repeat("m", 100)), true},
				{"mime 101 ascii", in("artifact", "mime", strings.Repeat("m", 101)), false},
				{"text_chars negative", in("artifact", "text_chars", -1), false},
				{"text_chars zero", in("artifact", "text_chars", 0), true},
				{"notes 20 entries of 300 multibyte runes", in("validation", "notes", stringList(20, cjk(300))), true},
				{"notes 21 entries", in("validation", "notes", stringList(21, "n")), false},
				{"notes entry 301 multibyte runes", in("validation", "notes", []any{cjk(301)}), false},
			})
		})
	}
}
