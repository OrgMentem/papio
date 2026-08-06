# Getting started

*papio* finds scholarly papers, checks each PDF is the paper you asked for, and
offers finished PDFs toward your reference library — into Zotero through
`zotio`, which always shows you a preview before it writes anything, or toward
papis, a plain folder, or your own script through a best-effort
[`on_ready` hook](hooks.md). Hook failures never fail or retry the acquisition job.

## 1. Install

*papio* has two parts: the **CLI & daemon** — it finds papers, validates each PDF, and offers finished artifacts for your library (Zotero through `zotio`, anywhere else through a best-effort [hook](hooks.md) handoff) — and a **browser extension** that hands off publisher-gated downloads from your logged-in browser. Install the CLI, load the extension, then wire them together with `papio init` ([step 2](#2-initialize-the-local-profile)).

### The CLI & daemon

*papio* runs on macOS, Linux, and Windows. For PDF validation it shells out to **Poppler** (`pdftotext`, for text-based identity checks) and, for scanned papers, **Tesseract** (OCR). Poppler is strongly recommended — without it papio can't read a PDF's text and flags candidates for manual review instead of auto-validating. Tesseract is optional: it only matters for image-only/scanned PDFs, and you can turn OCR off with `pdf.ocr_enabled = false`. Structural checks run inside the papio binary and need neither. Install the released binary plus these helpers:

=== "macOS"

    ```bash
    brew install orgmentem/tap/papio   # brew upgrade tracks new releases
    brew install poppler tesseract     # PDF validation + OCR
    ```

=== "Linux"

    **Distro packages** — every [GitHub release](https://github.com/OrgMentem/papio/releases) ships `.deb`, `.rpm`, and `.apk` for amd64/arm64. Download the file for your arch, then install it alongside Poppler/Tesseract:

    ```bash
    # Debian / Ubuntu
    sudo dpkg -i papio_<version>_linux_amd64.deb
    sudo apt install poppler-utils tesseract-ocr

    # Fedora / RHEL / openSUSE
    sudo rpm -i papio_<version>_linux_amd64.rpm
    sudo dnf install poppler-utils tesseract

    # Alpine
    sudo apk add --allow-untrusted papio_<version>_linux_amd64.apk
    sudo apk add poppler-utils tesseract-ocr
    ```

=== "Windows"

    **Scoop** — recommended: it tracks releases directly, so it is current the
    moment one is tagged:

    ```powershell
    scoop bucket add orgmentem https://github.com/OrgMentem/scoop-bucket
    scoop install papio               # scoop update papio tracks new releases
    scoop install poppler tesseract   # PDF validation + OCR
    ```

    **WinGet** — ships with Windows 10 and 11, but trails a release; see the note below:

    ```powershell
    winget install OrgMentem.papio
    winget install oschwartz10612.Poppler        # PDF validation
    winget install UB-Mannheim.TesseractOCR      # OCR (optional)
    ```

    !!! note "WinGet trails a release"
        Every papio release opens a pull request against `microsoft/winget-pkgs`,
        and WinGet serves the new version only once that PR merges — so it lands
        after the tag, and `winget upgrade OrgMentem.papio` finds nothing until
        then. That matters more here than for most tools: the browser extension
        enforces a minimum daemon version, so a daemon far enough behind makes the
        popup report that papio is out of date. If you see that, or you want
        releases as they land, use Scoop or the prebuilt archive below.

=== "Prebuilt binary"

    Every [GitHub release](https://github.com/OrgMentem/papio/releases) ships archives for macOS, Linux, and Windows (amd64/arm64) with cosign-signed checksums and SBOMs. Unpack and put `papio` on your `PATH`:

    - **macOS:** clear the Gatekeeper quarantine — `xattr -d com.apple.quarantine papio`, then `chmod +x papio`
    - **Linux:** `chmod +x papio`
    - **Windows:** unzip and add the folder to your `PATH`

    Install Poppler (and Tesseract for OCR) from your OS package manager — see the macOS / Linux / Windows tabs above.

=== "From source"

    ```sh
    go build ./cmd/papio
    ```

    Requires Poppler (and Tesseract for OCR) on your `PATH` — see the macOS / Linux / Windows tabs above.

Install `zotio` and put it on your `PATH` when you want *papio* to import finished PDFs into Zotero. **Not a Zotero user?** Skip it — answer `none` at the zotio prompt (or pass `--zotio-path ""`) and use a best-effort [`on_ready` hook](hooks.md) handoff instead; hook failures never fail or retry the acquisition job.

### The browser extension

*papio* bundles a browser extension that runs in any Chromium browser — Chrome, Edge, Vivaldi, Brave, Opera — or Firefox. It reaches the daemon through a native-messaging connector that `papio init` installs.

=== "Chrome / Chromium"

    1. Install [**papio** from the Chrome Web Store](https://chromewebstore.google.com/detail/papio/npccengdhjmpojpjmjoeeclpdhcjelhf). The store package keeps one fixed extension ID — `npccengdhjmpojpjmjoeeclpdhcjelhf` — across every Chromium browser, and `papio init` uses it by default, so there is nothing to copy.
    2. Open *papio*'s **Details** page and grant optional host permissions only for the publisher sites you use.

    Store-installed extensions update automatically. (If you install a differently keyed build — e.g. a future Edge Add-ons store copy, which carries a different ID — add its ID to `browser.extension_ids`.)

=== "Firefox"

    1. Install [**papio** from Firefox Add-ons](https://addons.mozilla.org/firefox/addon/papio/). The signed add-on carries the fixed ID `papio@orgmentem.com`, which `papio init` allows by default, so there is nothing to copy.
    2. On *papio*'s options page, grant the Library resolver access permission — Firefox treats host access as a runtime opt-in, so nothing is granted at install time.
    3. Grant the per-publisher host permissions for the sites you use, on the same page.

    Store-installed add-ons update automatically. Pass `--firefox-extension-id` to `papio init` only when the allowed add-on ID must differ — for example a locally built or self-signed copy. To load a development build instead, open `about:debugging#/runtime/this-firefox`, choose **Load Temporary Add-on**, and select `extension/firefox/manifest.json`; temporary add-ons do not persist across Firefox restarts.

=== "Development install (unpacked)"

    For hacking on the extension itself, load it unpacked from a checkout:

    1. Open `chrome://extensions`, enable **Developer mode**, choose **Load unpacked**, and select `extension/`.
    2. Hand the ID to `papio init --extension-id <id>` — or pass the extension *folder path* instead and *papio* computes the ID itself (macOS/Linux only: Chrome derives unpacked IDs from the load directory, but Windows hashes paths differently, so paste the literal ID there).

    Manually loaded builds do not auto-update; reload after each rebuild.

!!! note "Windows connector refresh"
    On Windows the connector is registered under the current-user registry (`HKCU\Software\{Google\Chrome,Mozilla}\NativeMessagingHosts`) and runs from a copy of the `papio` binary — rerun `papio init` after upgrading *papio* so that copy is refreshed. On macOS and Linux the connector is a symlink and needs no refresh.

## 2. Initialize the local profile

Run the guided first-run setup:

```sh
papio init
```

`papio init` writes your configuration, creates the data folder and its database,
checks the `zotio` program, installs the browser connector, and asks: “Check for
*papio* and zotio updates once a day? Queries GitHub releases only; nothing else
is sent. [Y/n]” It defaults to yes and then runs `doctor`. You can run it again
safely — it updates your setup without creating a second profile.

The interactive setup asks for:

1. A contact email for polite API pools.
2. The `zotio` executable and attachment mode (`stored` or `linked-file`).
   Answer `none` for the executable if you don't use Zotero; the
   attachment-mode question is then skipped and `papio doctor` reports zotio as
   `not configured (optional)` rather than failing.
3. Whether to install browser integration.
4. Browser extension identities: the Chrome extension ID (defaults to the
   Chrome Web Store package's fixed ID; only unpacked development builds need
   a different one) and the Firefox add-on ID, which defaults to the built
   add-on's fixed ID, `papio@orgmentem.com`.
5. Institution settings when browser integration is enabled: paste your library's
   discovery or search URL (Primo, SFX, WorldCat, or EBSCO) or the OpenURL base
   directly; *papio* derives the resolver base and, when present, the ProQuest
   account ID. If Zotero is installed with a resolver already configured,
   `papio init` offers it as the default. For scripted setup, use
   `--institution-url`.

```sh
papio init --institution-url "https://university.primo.exlibrisgroup.com/discovery/search?vid=UNIV:LIBRARY"
```

For an unattended setup, `--non-interactive` retains existing values unless a
flag overrides them. Use `--skip-browser` to omit Chrome/Firefox and browser-connector
setup:

```sh
papio init --non-interactive --email you@example.org --skip-browser
```

These flags set the corresponding setup values:

| Flag | Value |
| --- | --- |
| `--email` | Contact email for polite API pools. |
| `--zotio-path` | zotio executable path; `--zotio-path ""` disables Zotero integration. |
| `--attachment-mode` | `stored` or `linked-file`. |
| `--openurl-base` | Institution OpenURL resolver base URL. |
| `--shibboleth-entity-id` | Shibboleth IdP entityID for federated login-routing. |
| `--proquest-account-id` | A ProQuest account ID or URL containing `accountid=`. |
| `--extension-id` | Chrome extension ID permitted to reach the connector. |
| `--firefox-extension-id` | Firefox add-on ID permitted to reach the connector. |
| `--check-updates` | Allow a once-daily GitHub releases check for new *papio* and zotio versions (default `true`). |
| `--non-interactive` | Do not prompt; retain existing values unless a flag overrides them. |
| `--skip-browser` | Skip Chrome extension and connector setup. |

## 3. Check readiness

Run the health check after setup, especially after changing configuration:

```sh
papio doctor
```

It checks *papio* itself plus the pieces it depends on: your configuration, the
background service and browser extension, the connector, and zotio.

## 4. Acquire a first paper

Queue one DOI and wait for its job to settle:

```sh
papio acquire <doi> --wait
```

*papio* also ingests standard bibliographic batches—RIS, BibTeX, CSL-JSON, and
MEDLINE/NBIB—through `acquire --batch`:

```sh
papio acquire --batch refs.ris --label "thesis background"
```

A validated PDF is held in *papio*'s content-addressed artifact store. Choose
where to offer it:

=== "Zotero"

    ```sh
    papio zotio plan <job-id>                              # preview the exact changes
    papio zotio apply <plan-id> --confirm-sha256 <sha256>   # applies exactly that preview
    ```

    `acquire --auto-import` routes through the same plan/apply machinery.

=== "papis"

    ```toml
    [hooks]
    on_ready = 'papis add --from doi "$PAPIO_DOI" "$PAPIO_PDF"'
    ```

    This is a best-effort handoff. If the hook fails, *papio* records the
    failure without failing or retrying the acquisition job.

=== "Plain folder"

    ```toml
    [hooks]
    on_ready = 'cp "$PAPIO_PDF" "$HOME/Papers/"'
    ```

=== "Your own script"

    ```toml
    [hooks]
    on_ready = '/usr/local/bin/file-paper.sh'
    ```

    The job's identity arrives in `PAPIO_DOI`, `PAPIO_TITLE`, `PAPIO_PDF`, and
    `PAPIO_SHA256`; the full contract is in [filing & hooks](hooks.md).

Next, choose an [access mode](../concepts/access-modes.md), follow the
[User guide](user-guide.md) for discovery and browser handoffs, or review every
setting in the [configuration reference](../reference/config-reference.md).
