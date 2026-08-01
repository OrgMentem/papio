#!/usr/bin/env python3
"""Measure whether a non-Go consumer can reproduce a bundle's provenance_digest.

Evidence for ADR-0011. papio computes the digest as sha256 over
`json.Marshal` of the AcquisitionBundle struct with ProvenanceDigest blanked
(internal/bundle/export.go, func digest). That makes the canonicalisation
"whatever encoding/json does with this Go struct": field declaration order,
omitempty semantics, compact encoding, HTML escaping of & < >, and raw UTF-8
for everything else.

The question this answers is not "is that a good idea" but "can someone
outside Go actually verify it, and when does that stop working". Both halves
are measured rather than argued:

  1. Reproduction rate over a REAL corpus. Each bundle on disk already carries
     a digest Go computed, so it is ground truth. No Go needed.
  2. The encoder divergence matrix, observed by invoking Go directly rather
     than asserting its behaviour from documentation. Skipped if Go is absent.

Usage:  python3 dev/provenance-digest-reproducibility.py [bundle-dir]
Default bundle-dir is ~/.local/share/papio/bundles
"""

import glob
import hashlib
import json
import os
import subprocess
import sys
import tempfile

# json.dumps configurations a consumer would plausibly reach for. Key order is
# never sorted: Go emits struct field order, and Python preserves the insertion
# order produced by json.loads, so sorting is always wrong here.
CONFIGS = {
    "compact, ensure_ascii=True (json.dumps default escaping)": dict(separators=(",", ":")),
    "compact, ensure_ascii=False": dict(separators=(",", ":"), ensure_ascii=False),
    "default separators (spaces), ensure_ascii=False": dict(ensure_ascii=False),
    "compact, sort_keys=True": dict(separators=(",", ":"), sort_keys=True),
}


def reproduction_rate(bundle_dir):
    """For every real bundle, try to recompute the digest Go already recorded."""
    paths = sorted(glob.glob(os.path.join(bundle_dir, "*", "bundle.json")))
    if not paths:
        print(f"no bundles under {bundle_dir}")
        return

    escapable = non_ascii = 0
    hits = {name: 0 for name in CONFIGS}
    for path in paths:
        blob = open(path, "rb").read()
        if any(marker in blob for marker in (b"\\u0026", b"\\u003c", b"\\u003e")):
            escapable += 1
        if any(byte > 0x7F for byte in blob):
            non_ascii += 1

        doc = json.loads(blob)
        claimed = doc.get("provenance_digest", "")
        probe = dict(doc)
        probe["provenance_digest"] = ""
        for name, kwargs in CONFIGS.items():
            encoded = json.dumps(probe, **kwargs).encode()
            if "sha256:" + hashlib.sha256(encoded).hexdigest() == claimed:
                hits[name] += 1

    total = len(paths)
    print(f"corpus: {total} bundles")
    print(f"  containing Go-escaped & < >  : {escapable}")
    print(f"  containing non-ASCII         : {non_ascii}")
    print("\ndigest reproduced from the decoded document:")
    for name, n in hits.items():
        print(f"  {n:3}/{total}  {name}")


def divergence_matrix():
    """Observe Go's encoder directly rather than trusting documentation."""
    if not _have_go():
        print("\n(go not on PATH — skipping the observed divergence matrix)")
        return

    cases = {
        "plain ASCII": "Self-determination theory",
        "non-ASCII (diacritic)": "caf\u00e9 studies",
        "HTML-escapable (& in a URL)": "https://x.test/p?a=1&b=2",
        "both": "M\u00fcller & Sons",
    }
    program = """package main
import ("encoding/json";"fmt";"os")
func main(){
  var in []string
  _ = json.NewDecoder(os.Stdin).Decode(&in)
  out := make([]string, 0, len(in))
  for _, s := range in {
    b, _ := json.Marshal(s)
    out = append(out, string(b))
  }
  enc, _ := json.Marshal(out)
  fmt.Println(string(enc))
}"""
    with tempfile.TemporaryDirectory() as tmp:
        src = os.path.join(tmp, "main.go")
        open(src, "w").write(program)
        proc = subprocess.run(
            ["go", "run", src], input=json.dumps(list(cases.values())),
            capture_output=True, text=True, check=True,
        )
    go_out = json.loads(proc.stdout)

    print("\nobserved: Go json.Marshal vs Python json.dumps, per string")
    print(f"  {'case':<30}{'ensure_ascii=True':<20}ensure_ascii=False")
    for (label, value), go in zip(cases.items(), go_out):
        strict = json.dumps(value)
        loose = json.dumps(value, ensure_ascii=False)
        print(f"  {label:<30}"
              f"{'match' if strict == go else 'MISMATCH':<20}"
              f"{'match' if loose == go else 'MISMATCH'}")
    print("\n  No configuration reproduces Go once & < > appears: Go escapes them,")
    print("  Python never does. Non-ASCII needs ensure_ascii=False, which does not")
    print("  help with & < >, so the two hazards have no common setting.")


def _have_go():
    try:
        subprocess.run(["go", "version"], capture_output=True, check=True)
        return True
    except (OSError, subprocess.CalledProcessError):
        return False


if __name__ == "__main__":
    default = os.path.expanduser("~/.local/share/papio/bundles")
    reproduction_rate(sys.argv[1] if len(sys.argv) > 1 else default)
    divergence_matrix()
