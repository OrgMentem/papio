---
name: no-underscore-file-in-extension
description: "An underscore-prefixed filename anywhere under extension/ makes Chrome refuse the whole extension — including the user's daily browser"
condition: ['.*']
scope:
  - "tool:write(extension/**/_*)"
  - "tool:edit(extension/**/_*)"
interruptMode: always
---

**A `_`-prefixed file anywhere in an unpacked extension directory breaks the entire extension load.** Chrome refuses the manifest outright:

```
Cannot load extension with file or directory name _reload.html.
Filenames starting with "_" are reserved
```

So one leftover scratch file in `extension/` or `extension/dev-unpacked/` **bricks an extension reload — including the user's daily browser** — and nothing in the error points at the temp file as the cause beyond that single line, which is easy to miss while looking for a code fault.

Name the file without the leading underscore (`scratch-reload.html`, `tmp-reload.html`), or put it somewhere outside the extension directory entirely — `dev/scratch/` is gitignored and exists for exactly this.
