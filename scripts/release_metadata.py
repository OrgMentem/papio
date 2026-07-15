#!/usr/bin/env python3
"""Deterministic metadata operations for scripts/release.sh."""

from __future__ import annotations

import argparse
import base64
import hashlib
import re
import json
import os
from pathlib import Path
import sys
from typing import Any, Iterable


CHROME_ID_ALPHABET = "abcdefghijklmnop"
MANIFEST_NAME = "release-manifest.json"
CHECKSUMS_NAME = "SHA256SUMS"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def release_files(root: Path, excluded: set[str]) -> Iterable[Path]:
    for path in sorted(root.rglob("*")):
        if path.is_file() and path.relative_to(root).as_posix() not in excluded:
            yield path


def pinned_extension_id(public_key_der: bytes) -> str:
    key_hash = hashlib.sha256(public_key_der).digest()
    return "".join(
        CHROME_ID_ALPHABET[nibble]
        for byte in key_hash[:16]
        for nibble in (byte >> 4, byte & 0x0F)
    )


def pin_manifest(args: argparse.Namespace) -> None:
    manifest_path = Path(args.manifest)
    key_path = Path(args.key_file)
    try:
        key = "".join(key_path.read_text(encoding="utf-8").split())
        public_key_der = base64.b64decode(key, validate=True)
    except (OSError, ValueError) as exc:
        raise SystemExit(f"could not read a valid manifest public key: {exc}") from exc

    if not public_key_der:
        raise SystemExit("manifest public key is empty")

    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"could not read extension manifest: {exc}") from exc

    manifest["key"] = key
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    Path(args.id_file).write_text(pinned_extension_id(public_key_der) + "\n", encoding="utf-8")


def manifest_version(args: argparse.Namespace) -> None:
    try:
        manifest = json.loads(Path(args.manifest).read_text(encoding="utf-8"))
        version = manifest["version"]
    except (OSError, json.JSONDecodeError, KeyError, TypeError) as exc:
        raise SystemExit(f"could not read extension manifest version: {exc}") from exc
    if not isinstance(version, str) or not version:
        raise SystemExit("extension manifest version must be a non-empty string")
    print(version)

def go_const_version(args: argparse.Namespace) -> None:
    try:
        source = Path(args.source).read_text(encoding="utf-8")
    except OSError as exc:
        raise SystemExit(f"could not read Go version source: {exc}") from exc
    match = re.search(r'const\s+Version\s*=\s*"([^"]+)"', source)
    if match is None:
        raise SystemExit("could not find const Version in Go version source")
    print(match.group(1))




def normalize_timestamps(args: argparse.Namespace) -> None:
    root = Path(args.directory)
    for path in sorted(root.rglob("*"), reverse=True):
        os.utime(path, (args.epoch, args.epoch), follow_symlinks=False)
    os.utime(root, (args.epoch, args.epoch), follow_symlinks=False)


def verify_extension_archive(args: argparse.Namespace) -> None:
    import zipfile

    with zipfile.ZipFile(args.zip_path) as archive:
        names = archive.namelist()
    if "manifest.json" not in names:
        raise SystemExit("extension archive is missing manifest.json")
    if not any(name.startswith("dist/") and not name.endswith("/") for name in names):
        raise SystemExit("extension archive is missing built dist files")
    unexpected = [
        name
        for name in names
        if name != "manifest.json"
        and not name.startswith("dist/")
        and not name.startswith("icons/")
    ]
    if unexpected:
        raise SystemExit(
            "extension archive contains files outside manifest.json, dist/, and icons/: "
            + ", ".join(unexpected)
        )


def fallback_licenses(args: argparse.Namespace) -> None:
    decoder = json.JSONDecoder()
    source = sys.stdin.read()
    offset = 0
    modules: list[dict[str, Any]] = []
    while offset < len(source):
        while offset < len(source) and source[offset].isspace():
            offset += 1
        if offset == len(source):
            break
        value, offset = decoder.raw_decode(source, offset)
        if isinstance(value, dict):
            modules.append(value)

    rows = ["module,version,license"]
    for module in modules:
        path = str(module.get("Path", ""))
        version = str(module.get("Version", ""))
        rows.append(f"{path},{version},UNKNOWN (go-licenses unavailable)")
    Path(args.output).write_text("\n".join(rows) + "\n", encoding="utf-8")


def release_manifest(args: argparse.Namespace) -> None:
    root = Path(args.directory)
    files = [
        {
            "path": path.relative_to(root).as_posix(),
            "sha256": sha256(path),
            "size_bytes": path.stat().st_size,
        }
        for path in release_files(root, {MANIFEST_NAME, CHECKSUMS_NAME})
    ]
    data: dict[str, Any] = {
        "schema_version": 1,
        "release_version": args.version,
        "target": "darwin-arm64",
        "components": {
            "papio": {
                "commit": args.papio_commit,
                "version": args.papio_version,
                "version_source": "internal/api/handler.go: const Version",
                "version_injected": False,
                "version_injection_note": "Papio does not yet expose an ldflags-overridable version.",
                "artifact": "papio-darwin-arm64",
            },
            "zotio": {
                "commit": args.zotio_commit,
                "version": args.version,
                "version_injected": True,
                "version_ldflag": "zotio/internal/cli.version",
                "artifact": "zotio-darwin-arm64",
            },
            "extension": {
                "version": args.extension_version,
                "artifact": f"papio-extension-{args.version}.zip",
                "pinned": bool(args.extension_id),
                "extension_id": args.extension_id or None,
                "extension_id_derivation": (
                    "SHA-256(public manifest key DER), first 16 bytes, "
                    "nibbles mapped a-p"
                    if args.extension_id
                    else None
                ),
            },
        },
        "license_inventory": {"method": args.license_method},
        "files": files,
    }
    Path(args.output).write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")


def checksums(args: argparse.Namespace) -> None:
    root = Path(args.directory)
    lines = [
        f"{sha256(path)}  {path.relative_to(root).as_posix()}"
        for path in release_files(root, {CHECKSUMS_NAME})
    ]
    (root / CHECKSUMS_NAME).write_text("\n".join(lines) + "\n", encoding="utf-8")


def parser() -> argparse.ArgumentParser:
    command_parser = argparse.ArgumentParser()
    commands = command_parser.add_subparsers(dest="command", required=True)

    pin = commands.add_parser("pin-manifest")
    pin.add_argument("--manifest", required=True)
    pin.add_argument("--key-file", required=True)
    pin.add_argument("--id-file", required=True)
    pin.set_defaults(func=pin_manifest)

    version = commands.add_parser("manifest-version")
    version.add_argument("--manifest", required=True)
    version.set_defaults(func=manifest_version)

    go_version = commands.add_parser("go-const-version")
    go_version.add_argument("--source", required=True)
    go_version.set_defaults(func=go_const_version)

    timestamps = commands.add_parser("normalize-timestamps")
    timestamps.add_argument("--directory", required=True)
    timestamps.add_argument("--epoch", required=True, type=int)
    timestamps.set_defaults(func=normalize_timestamps)

    archive = commands.add_parser("verify-extension-archive")
    archive.add_argument("--zip-path", required=True)
    archive.set_defaults(func=verify_extension_archive)

    fallback = commands.add_parser("fallback-licenses")
    fallback.add_argument("--output", required=True)
    fallback.set_defaults(func=fallback_licenses)

    metadata = commands.add_parser("release-manifest")
    metadata.add_argument("--directory", required=True)
    metadata.add_argument("--output", required=True)
    metadata.add_argument("--version", required=True)
    metadata.add_argument("--papio-commit", required=True)
    metadata.add_argument("--papio-version", required=True)
    metadata.add_argument("--zotio-commit", required=True)
    metadata.add_argument("--extension-version", required=True)
    metadata.add_argument("--extension-id", default="")
    metadata.add_argument("--license-method", required=True)
    metadata.set_defaults(func=release_manifest)

    checksum = commands.add_parser("checksums")
    checksum.add_argument("--directory", required=True)
    checksum.set_defaults(func=checksums)

    return command_parser


def main() -> None:
    args = parser().parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
