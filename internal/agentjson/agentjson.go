// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package agentjson holds the one page-envelope contract every machine-readable
// papio surface emits, so the CLI's `--json` and the MCP resources cannot drift
// apart. One catalog, no drift — the same reasoning as internal/errcat.
//
// The shape is deliberately boring and fixed at exactly two keys:
//
//	{"jobs": [...], "truncated": false}
//
// A named row key rather than a bare top-level array, because an array leaves a
// consumer nowhere to put "there were more rows than this" and nowhere to add a
// field later without a breaking change. Agents are the primary consumer of this
// output; a shape that has to be reverse-engineered per command is a defect in
// the interface, not a documentation gap.
//
// Two invariants matter more than they look:
//
//   - An empty result marshals as `[]`, never `null`. A nil Go slice would
//     otherwise reach an agent as `null` and break the obvious `for row in
//     payload["jobs"]`, which is exactly the kind of shape drift this package
//     exists to stop.
//   - `truncated` is always present, even when false, so a consumer never has to
//     distinguish "not truncated" from "this surface forgot to say".
package agentjson

import "encoding/json"

// Page is one bounded result set: the rows under their own key, plus whether the
// row cap hid anything. Build it with Envelope so the invariants hold.
type Page struct {
	key       string
	rows      any
	truncated bool
}

// Envelope pairs rows with the key they appear under. key names the collection
// ("jobs", "works", "watches"), matching the command that produced it. rows is
// generic over the row type rather than `any` so a caller can no longer hand
// it a non-slice value (a single struct, a map) by mistake — every envelope
// page is a list of something, by construction.
func Envelope[T any](key string, rows []T, truncated bool) Page {
	return Page{key: key, rows: rows, truncated: truncated}
}

// MarshalJSON emits exactly the two contract keys, rows first.
//
// Normalizing `null` to `[]` happens here rather than only in Truncate/Capped
// because this is the single point every surface passes through; a caller that
// hands us a nil slice directly must still produce a list.
func (p Page) MarshalJSON() ([]byte, error) {
	rows, err := json.Marshal(p.rows)
	if err != nil {
		return nil, err
	}
	if string(rows) == "null" {
		rows = []byte("[]")
	}
	key, err := json.Marshal(p.key)
	if err != nil {
		return nil, err
	}
	const truncatedKey = `,"truncated":`
	out := make([]byte, 0, len(key)+len(rows)+len(truncatedKey)+7)
	out = append(out, '{')
	out = append(out, key...)
	out = append(out, ':')
	out = append(out, rows...)
	out = append(out, truncatedKey...)
	if p.truncated {
		out = append(out, "true"...)
	} else {
		out = append(out, "false"...)
	}
	return append(out, '}'), nil
}

// Truncate caps rows and reports whether anything was dropped. A nil slice
// becomes an empty one so the payload never carries `null` where a consumer
// expects a list.
//
// cap <= 0 means "no cap": the rows pass through untruncated, normalized.
func Truncate[T any](rows []T, cap int) ([]T, bool) {
	if rows == nil {
		return []T{}, false
	}
	if cap <= 0 || len(rows) <= cap {
		return rows, false
	}
	return rows[:cap], true
}

// Capped is the common path for a command with a --limit flag: the daemon was
// asked for at most limit rows, so receiving exactly that many means more may
// exist upstream. It reports the rows normalized against `null` alongside that
// judgement.
//
// This is deliberately a "may be more", not a proof. The alternative — asking
// every daemon query for limit+1 rows to know for certain — buys precision no
// consumer needs, since the remedy is the same either way: raise --limit or
// paginate.
func Capped[T any](rows []T, limit int) ([]T, bool) {
	if rows == nil {
		return []T{}, false
	}
	if limit <= 0 {
		return rows, false
	}
	return rows, len(rows) >= limit
}
