// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// bun test preload (extension/bunfig.toml [test] preload): makes EVERY
// happy-dom Window in the suite hermetic, including the tests that build
// `new Window(...)` directly with default settings. See ./hermetic-dom.ts.
import { installHermeticDefaults } from "./hermetic-dom";

installHermeticDefaults();
