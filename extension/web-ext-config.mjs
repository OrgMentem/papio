// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// web-ext configuration for the Firefox dev loop (`bun run dev`).
//
// firefoxProfile MUST be an absolute path, not a bare name: a name makes web-ext
// pass `-P <name>`, and an unregistered name pops Firefox's profile-chooser modal
// that blocks the debugger server (ECONNREFUSED). An absolute path makes it pass
// `-profile <dir>`, which boots straight into a persistent, gitignored dev
// profile — so granted permissions and institutional logins survive reloads and
// restarts. Nothing here uses WebDriver/Marionette (web-ext installs over the
// devtools RDP), so the dev browser never sets navigator.webdriver.

import { fileURLToPath } from "node:url";

const here = (relative) => fileURLToPath(new URL(relative, import.meta.url));

export default {
  sourceDir: here("firefox"),
  run: {
    firefox: "firefoxdeveloperedition",
    firefoxProfile: here(".ff-dev-profile"),
    profileCreateIfMissing: true,
    keepProfileChanges: true,
  },
};
