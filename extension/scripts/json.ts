// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The release scripts read JSON from Google's and Mozilla's store APIs.
// Nothing about those payloads is compiler-known, so every field is checked at
// the boundary; this is the one place that turns an unknown into something
// index-able, so the checked cast lives here instead of at each field read.

export function record(value: unknown): Record<string, unknown> | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  // Checked directly above: a non-null, non-array object is an index-able record.
  const indexable = value as Record<string, unknown>;
  return indexable;
}
