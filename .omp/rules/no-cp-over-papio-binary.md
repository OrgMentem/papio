---
name: no-cp-over-papio-binary
description: "Never cp over an existing papio binary on macOS — reusing the inode poisons the kernel signature cache and the next exec dies with SIGKILL"
condition:
  - '\b(cp|install)\b[^\n]*(bin/papio|papio-native-host)\b'
  - '\bcp\b[^\n]*\bpapio\b\s*$'
scope: ["tool:bash"]
interruptMode: always
---

**Do not `cp` over an existing papio binary on macOS.** Overwriting the inode of a previously-executed signed binary poisons the kernel's signature cache, and the next `exec` dies with **SIGKILL (exit 137)** — with no message naming the cause. Nothing about the failure points back at the copy.

Use `mv` into place, or `rm` the target first, so the new binary gets a fresh inode.

Better still, for local daemon changes use **`make dev-deploy`** rather than hand-rolling the dance: it builds a version-stamped binary, installs it to a stable `~/.local/bin/papio`, repoints the native-host symlink there, restarts the host and daemon, rebuilds the extension, and runs `doctor`. Skipping the `native-host install` step by hand leaves the extension talking to a stale or dead host.

Remember this machine has **two** papio binaries — `/opt/homebrew/bin/papio` (CLI/daemon on PATH) and `~/.local/bin/papio` (target of the native-messaging symlink `~/.config/papio/bin/papio-native-host`). Updating only the first leaves browsers spawning the OLD native host, which shows up as a `legacy` browser session and as `papio doctor` failing its native-host version check.

Copying a papio binary *somewhere new* (a fresh path, a tarball staging dir, a temp dir) is fine — this rule is about overwriting a path that has already been executed.
