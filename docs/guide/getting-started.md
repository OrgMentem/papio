# Getting started

*papio* finds scholarly papers, checks each PDF is the paper you asked for, and
hands finished PDFs to your Zotero library through `zotio` — which always shows
you a preview before it writes anything.

## 1. Prerequisites

*papio* currently runs on macOS. Install a Go-built `papio` binary, or build it
from this checkout:

```sh
go build ./cmd/papio
```

PDF validation and OCR use Poppler and Tesseract:

```sh
brew install poppler tesseract
```

Install Chrome or Firefox to use the browser extension. Install `zotio` and
make it available on `PATH` only when you want *papio* to import finished PDFs
into Zotero.

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
4. Browser extension identities: the Chrome extension ID from
   `chrome://extensions` and the Firefox add-on ID. Firefox defaults to the
   built add-on's fixed ID, `papio@orgmentem.com`.
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

## 3. Load the extension

`papio init` prints the exact browser setup steps after attempting native-host
installation. Load the unpacked extension in the browser you use.

### Chrome

1. Open `chrome://extensions`.
2. Enable **Developer mode**, then choose **Load unpacked** and select
   `extension/`.
3. Open *papio*'s **Details** page and grant optional host permissions only for
   publisher sites you use.
4. If Chrome shows an ID different from the one configured during setup, rerun:

   ```sh
   papio init --extension-id <id>
   ```

### Firefox

1. Open `about:debugging#/runtime/this-firefox` and choose **Load Temporary
   Add-on**.
2. Select `extension/firefox/manifest.json`.
3. On *papio*'s options page, grant the Library resolver access permission.

The built Firefox extension uses `papio@orgmentem.com` by default. Pass
`--firefox-extension-id` only when the allowed add-on ID must differ.

## 4. Check readiness

Run the health check after setup, especially after changing configuration:

```sh
papio doctor
```

It checks papio itself plus the pieces it depends on: your configuration, the
background service and browser extension, the connector, and zotio.

## 5. Acquire a first paper

Queue one DOI and wait for its job to settle:

```sh
papio acquire <doi> --wait
```

Next, choose an [access mode](../concepts/access-modes.md), follow the
[User guide](user-guide.md) for discovery and browser handoffs, or review every
setting in the [configuration reference](../reference/config-reference.md).
