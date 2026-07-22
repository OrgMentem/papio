# Getting started

*papio* finds scholarly papers, checks each PDF is the paper you asked for, and
hands finished PDFs to your Zotero library through `zotio` — which always shows
you a preview before it writes anything.

## 1. Install

*papio* has two parts: the **CLI & daemon** — it finds papers, validates each PDF, and files finished artifacts into Zotero through `zotio` — and a **browser extension** that hands off publisher-gated downloads from your logged-in browser. Install the CLI, load the extension, then wire them together with `papio init` ([step 2](#2-initialize-the-local-profile)).

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

    ```powershell
    scoop bucket add orgmentem https://github.com/OrgMentem/scoop-bucket
    scoop install papio               # scoop update papio tracks new releases
    scoop install poppler tesseract   # PDF validation + OCR
    ```

    !!! note "WinGet is on the way"
        A `winget install OrgMentem.papio` manifest is pending review in `microsoft/winget-pkgs`. Until it lands, use Scoop or a prebuilt archive.

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

Install `zotio` and put it on your `PATH` when you want *papio* to import finished PDFs into Zotero.

### The browser extension

*papio* bundles a browser extension that runs in any Chromium browser — Chrome, Edge, Vivaldi, Brave, Opera — or Firefox. It reaches the daemon through a native-messaging connector that `papio init` installs.

=== "Chrome / Chromium"

    1. Install [**papio** from the Chrome Web Store](https://chromewebstore.google.com/detail/papio/npccengdhjmpojpjmjoeeclpdhcjelhf). The store package keeps one fixed extension ID — `npccengdhjmpojpjmjoeeclpdhcjelhf` — across every Chromium browser, and `papio init` uses it by default, so there is nothing to copy.
    2. Open *papio*'s **Details** page and grant optional host permissions only for the publisher sites you use.

    Store-installed extensions update automatically. (If you install a differently keyed build — e.g. a future Edge Add-ons store copy, which carries a different ID — add its ID to `browser.extension_ids`.)

=== "Firefox"

    The add-on is awaiting review on Firefox Add-ons (AMO); until it is published, load the built add-on temporarily:

    1. Open `about:debugging#/runtime/this-firefox` and choose **Load Temporary Add-on**.
    2. Select `extension/firefox/manifest.json` (from a release bundle or a local `bun run build`).
    3. On *papio*'s options page, grant the Library resolver access permission.

    Temporary add-ons do not persist across Firefox restarts. The built add-on uses the fixed ID `papio@orgmentem.com` by default; pass `--firefox-extension-id` to `papio init` only when the allowed add-on ID must differ.

=== "Development install (unpacked)"

    For hacking on the extension itself, load it unpacked from a checkout:

    1. Open `chrome://extensions`, enable **Developer mode**, choose **Load unpacked**, and select `extension/`.
    2. Note the extension ID shown on the card — unpacked builds get a machine-specific ID, so hand it to `papio init --extension-id <id>`.

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
3. Whether to install browser integration.
4. Browser extension identities: the Chrome extension ID (defaults to the
   Chrome Web Store package's fixed ID; only unpacked development builds need
   a different one) and the Firefox add-on ID, which defaults to the built
   add-on's fixed ID, `papio@orgmentem.com`.
5. Institution settings when browser integration is enabled: a library OpenURL
   resolver base URL; if that URL is set, an optional Shibboleth IdP entityID
   for auto login-routing and a ProQuest account ID. For ProQuest, paste either
   the numeric ID or a ProQuest URL containing `accountid=`; *papio* extracts the
   account ID from the URL.

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
| `--zotio-path` | zotio executable path. |
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

Next, choose an [access mode](../concepts/access-modes.md), follow the
[User guide](user-guide.md) for discovery and browser handoffs, or review every
setting in the [configuration reference](../reference/config-reference.md).
