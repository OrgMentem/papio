# Getting started

Papio is a local paper-acquisition broker. It creates bounded acquisition jobs,
validates candidate PDFs, and sends ready artifacts to Zotio only through its
preview-and-confirmation boundary.

## 1. Prerequisites

Papio currently runs on macOS. Install a Go-built `papio` binary, or build it
from this checkout:

```sh
go build ./cmd/papio
```

PDF validation and OCR use Poppler and Tesseract:

```sh
brew install poppler tesseract
```

Install Chrome or Firefox to use the browser extension. Install `zotio` and
make it available on `PATH` only when you want Papio to import ready artifacts
into Zotero.

## 2. Initialize the local profile

Run the guided first-run setup:

```sh
papio init
```

`papio init` writes validated configuration, creates the data directory and
applies database migrations, checks the Zotio executable, installs the native
messaging host, and runs `doctor`. It is idempotent, so rerunning it updates
setup without creating a separate profile.

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
   the numeric ID or a ProQuest URL containing `accountid=`; Papio extracts the
   account ID from the URL.

For an unattended setup, `--non-interactive` retains existing values unless a
flag overrides them. Use `--skip-browser` to omit Chrome/Firefox and native-host
setup:

```sh
papio init --non-interactive --email you@example.org --skip-browser
```

These flags set the corresponding setup values:

| Flag | Value |
| --- | --- |
| `--email` | Contact email for polite API pools. |
| `--zotio-path` | Zotio executable path. |
| `--attachment-mode` | `stored` or `linked-file`. |
| `--openurl-base` | Institution OpenURL resolver base URL. |
| `--shibboleth-entity-id` | Shibboleth IdP entityID for federated login-routing. |
| `--proquest-account-id` | A ProQuest account ID or URL containing `accountid=`. |
| `--extension-id` | Chrome extension ID permitted to reach the native host. |
| `--firefox-extension-id` | Firefox add-on ID permitted to reach the native host. |
| `--non-interactive` | Do not prompt; retain existing values unless a flag overrides them. |
| `--skip-browser` | Skip Chrome extension and native-host setup. |

## 3. Load the extension

`papio init` prints the exact browser setup steps after attempting native-host
installation. Load the unpacked extension in the browser you use.

### Chrome

1. Open `chrome://extensions`.
2. Enable **Developer mode**, then choose **Load unpacked** and select
   `extension/`.
3. Open Papio's **Details** page and grant optional host permissions only for
   publisher sites you use.
4. If Chrome shows an ID different from the one configured during setup, rerun:

   ```sh
   papio init --extension-id <id>
   ```

### Firefox

1. Open `about:debugging#/runtime/this-firefox` and choose **Load Temporary
   Add-on**.
2. Select `extension/firefox/manifest.json`.
3. On Papio's options page, grant the Library resolver access permission.

The built Firefox extension uses `papio@orgmentem.com` by default. Pass
`--firefox-extension-id` only when the allowed add-on ID must differ.

## 4. Check readiness

Run the health check after setup, especially after changing configuration:

```sh
papio doctor
```

## 5. Acquire a first paper

Queue one DOI and wait for its job to settle:

```sh
papio acquire <doi> --wait
```

Next, choose an [access mode](../concepts/access-modes.md), follow the
[User guide](user-guide.md) for discovery and browser handoffs, or review every
setting in the [configuration reference](../reference/config-reference.md).
