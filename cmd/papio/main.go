// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio — legitimate paper-acquisition broker (Phase 0 scaffold).
//
// The full CLI (acquire, jobs, actions, artifacts, zotio, doctor, daemon, mcp)
// lands in Phase 1. This stub exists so the module builds as a binary and so
// the executable-basename dispatch contract is pinned from day one: Chrome's
// native-host manifest points at a `papio-native-host` link to this binary,
// and dispatch happens on os.Args[0]'s basename, never on a subcommand Chrome
// cannot pass.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"papio/internal/protocol"
)

const version = "0.0.0-dev"

func main() {
	switch filepath.Base(os.Args[0]) {
	case "papio-native-host":
		// Chrome invokes the native host with the extension origin as the first
		// argument. The bridge is implemented in Phase 2; refuse loudly so a
		// premature install cannot half-work. stdout stays reserved for framed
		// native messages, so this goes to stderr.
		fmt.Fprintln(os.Stderr, "papio-native-host: bridge not implemented yet (Phase 2); protocol "+protocol.BrowserProtocolVersion)
		os.Exit(1)
	default:
		fmt.Printf("papio %s (Phase 0 scaffold; contracts: %s, %s, %s)\n",
			version, protocol.WorkRequestSchemaVersion, protocol.AcquisitionBundleSchemaVersion, protocol.BrowserProtocolVersion)
	}
}
