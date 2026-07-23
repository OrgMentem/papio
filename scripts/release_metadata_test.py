#!/usr/bin/env python3
"""Regression tests for direct-distribution release metadata."""

from __future__ import annotations

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "release_metadata.py"


class ReleaseManifestTest(unittest.TestCase):
    def test_records_the_bundled_zotio_version(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            output = root / "release-manifest.json"
            subprocess.run(
                [
                    sys.executable,
                    str(SCRIPT),
                    "release-manifest",
                    "--directory",
                    str(root),
                    "--output",
                    str(output),
                    "--version",
                    "0.10.1",
                    "--papio-commit",
                    "papio-commit",
                    "--papio-version",
                    "0.10.1",
                    "--zotio-commit",
                    "zotio-commit",
                    "--zotio-version",
                    "0.13.0",
                    "--extension-version",
                    "0.5.0",
                    "--license-method",
                    "test",
                ],
                check=True,
                cwd=ROOT,
            )
            manifest = json.loads(output.read_text(encoding="utf-8"))

        self.assertEqual(manifest["components"]["papio"]["version"], "0.10.1")
        self.assertEqual(manifest["components"]["zotio"]["version"], "0.13.0")


if __name__ == "__main__":
    unittest.main()
