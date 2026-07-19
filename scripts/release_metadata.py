#!/usr/bin/env python3
"""Deterministic metadata operations for scripts/release.sh."""

from __future__ import annotations

import argparse
import base64
import hashlib
import re
import json
import subprocess
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
    match = re.search(r'(?:const|var)\s+Version\s*=\s*"([^"]+)"', source)
    if match is None:
        raise SystemExit("could not find const Version in Go version source")
    print(match.group(1))

SEMVER_CORE_RE = re.compile(
    r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$"
)


def semver_core(version: str) -> tuple[int, int, int]:
    match = SEMVER_CORE_RE.fullmatch(version)
    if match is None:
        raise ValueError(f"must be x.y.z semver: {version!r}")
    return (int(match.group(1)), int(match.group(2)), int(match.group(3)))


def read_version(path: Path, name: str) -> str:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))["version"]
    except (OSError, json.JSONDecodeError, KeyError, TypeError) as exc:
        raise SystemExit(f"could not read {name} version from {path}: {exc}") from exc
    if not isinstance(value, str) or not value:
        raise SystemExit(f"{name} version in {path} must be a non-empty string")
    return value


def read_const_version(root: Path, relative_path: str, const_name: str) -> str:
    path = root / relative_path
    try:
        source = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise SystemExit(f"could not read {const_name} source {path}: {exc}") from exc
    match = re.search(
        rf'^\s*(?:const\s+)?{re.escape(const_name)}\s*=\s*"([^"]+)"',
        source,
        flags=re.MULTILINE,
    )
    if match is None:
        raise SystemExit(f"could not find {const_name} const in {path}")
    return match.group(1)


def bundled_zotio_version(binary: str) -> str:
    try:
        result = subprocess.run(
            [binary, "version", "--agent"],
            check=False,
            capture_output=True,
            text=True,
        )
    except OSError as exc:
        raise ValueError(f"could not run bundled zotio binary {binary}: {exc}") from exc
    if result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip() or f"exit {result.returncode}"
        raise ValueError(f"bundled zotio version command failed: {detail}")
    # Zotio prints a plain "zotio X.Y.Z" line (see papio's own runtime preflight
    # in internal/zotio/client.go), not JSON. Parse the same shape here so the
    # release check matches what the daemon actually validates.
    match = re.match(r"^zotio (\d+\.\d+\.\d+(?:[-+][^ \n]+)?)", result.stdout.strip())
    if not match:
        raise ValueError(
            "bundled zotio version command did not return a 'zotio X.Y.Z' line: "
            f"{result.stdout.strip()!r}"
        )
    return match.group(1)


def compat(args: argparse.Namespace) -> None:
    root = Path(args.repo_root)
    manifest_version_value = read_version(root / "extension/manifest.json", "manifest")
    package_version_value = read_version(root / "extension/package.json", "package")
    min_extension_version = read_const_version(
        root, "internal/browser/bridge.go", "MinExtensionVersion"
    )
    min_daemon_version = read_const_version(
        root, "extension/src/background.ts", "MIN_DAEMON_VERSION"
    )
    minimum_zotio_version = read_const_version(
        root, "internal/zotio/client.go", "MinimumVersion"
    )

    versions = {
        "manifest": manifest_version_value,
        "package": package_version_value,
        "MinExtensionVersion": min_extension_version,
        "MIN_DAEMON_VERSION": min_daemon_version,
        "MinimumVersion": minimum_zotio_version,
    }
    if args.papio_version is not None:
        versions["papio"] = args.papio_version

    zotio_version: str | None = None
    zotio_error: str | None = None
    if not args.skip_zotio:
        if args.zotio_binary is None:
            raise SystemExit("compat requires --zotio-binary unless --skip-zotio is set")
        try:
            zotio_version = bundled_zotio_version(args.zotio_binary)
            versions["zotio"] = zotio_version
        except ValueError as exc:
            zotio_error = str(exc)

    parsed_versions: dict[str, tuple[int, int, int]] = {}
    format_failures: list[str] = []
    for name, version in versions.items():
        try:
            parsed_versions[name] = semver_core(version)
        except ValueError as exc:
            format_failures.append(f"{name} {exc}")

    failures: list[str] = []

    def check(name: str, passed: bool, detail: str) -> None:
        print(f"{'PASS' if passed else 'FAIL'} {name}: {detail}")
        if not passed:
            failures.append(name)

    check(
        "version format",
        not format_failures,
        "all versions are valid x.y.z semver"
        if not format_failures
        else "; ".join(format_failures),
    )
    check(
        "extension package version",
        manifest_version_value == package_version_value,
        f"manifest {manifest_version_value} == package {package_version_value}",
    )

    extension_floor_ready = {
        "MinExtensionVersion",
        "manifest",
    }.issubset(parsed_versions)
    check(
        "daemon extension floor",
        extension_floor_ready
        and parsed_versions["MinExtensionVersion"] <= parsed_versions["manifest"],
        f"MinExtensionVersion {min_extension_version} <= manifest {manifest_version_value}"
        if extension_floor_ready
        else "cannot compare invalid version",
    )

    if args.papio_version is not None:
        daemon_floor_ready = {
            "MIN_DAEMON_VERSION",
            "papio",
        }.issubset(parsed_versions)
        check(
            "extension daemon floor",
            daemon_floor_ready
            and parsed_versions["MIN_DAEMON_VERSION"] <= parsed_versions["papio"],
            f"MIN_DAEMON_VERSION {min_daemon_version} <= papio {args.papio_version}"
            if daemon_floor_ready
            else "cannot compare invalid version",
        )

    if not args.skip_zotio:
        zotio_floor_ready = zotio_error is None and {
            "zotio",
            "MinimumVersion",
        }.issubset(parsed_versions)
        check(
            "bundled zotio floor",
            zotio_floor_ready
            and parsed_versions["zotio"] >= parsed_versions["MinimumVersion"],
            f"zotio {zotio_version} >= MinimumVersion {minimum_zotio_version}"
            if zotio_floor_ready
            else zotio_error or "cannot compare invalid version",
        )

    if failures:
        raise SystemExit(f"compatibility preflight failed: {', '.join(failures)}")


def bump_extension(args: argparse.Namespace) -> None:
    """Set the extension version in both files that must move together.

    The extension version lives in extension/manifest.json AND
    extension/package.json; the `compat` preflight fails CI when they differ.
    Rewrites are regex-targeted to the top-level "version" key so the rest of
    each file's formatting is untouched.
    """
    version = args.version
    try:
        semver_core(version)
    except ValueError as exc:
        raise SystemExit(f"bump-extension: {exc}")
    root = Path(args.repo_root)
    version_re = re.compile(r'^(\s*"version"\s*:\s*")([^"]+)(")', re.MULTILINE)
    for relative in ("extension/manifest.json", "extension/package.json"):
        path = root / relative
        text = path.read_text(encoding="utf-8")
        updated, count = version_re.subn(
            lambda m: f"{m.group(1)}{version}{m.group(3)}", text, count=1
        )
        if count != 1:
            raise SystemExit(f"bump-extension: no version field found in {relative}")
        old = version_re.search(text).group(2)
        path.write_text(updated, encoding="utf-8")
        print(f"{relative}: {old} -> {version}")




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
                "version_source": "internal/api/handler.go: var Version",
                "version_injected": True,
                "version_ldflag": "papio/internal/api.Version",
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
                "artifacts": {
                    "chrome": f"papio-extension-{args.version}.zip",
                    "firefox": f"papio-extension-firefox-{args.version}.zip",
                },
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

    compatibility = commands.add_parser(
        "compat",
        help="check compatibility across the release artifacts",
    )
    compatibility.add_argument("--repo-root", required=True)
    compatibility.add_argument(
        "--papio-version",
        help="papio version being released; omit to skip the extension daemon floor",
    )
    compatibility.add_argument(
        "--zotio-binary",
        help="path to the zotio binary being bundled",
    )
    compatibility.add_argument(
        "--skip-zotio",
        action="store_true",
        help="skip the bundled zotio minimum-version check",
    )
    compatibility.set_defaults(func=compat)

    bump = commands.add_parser(
        "bump-extension",
        help="set the extension version in manifest.json and package.json together",
    )
    bump.add_argument("--repo-root", required=True)
    bump.add_argument("--version", required=True)
    bump.set_defaults(func=bump_extension)

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
