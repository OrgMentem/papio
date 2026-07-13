// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio — legitimate paper-acquisition broker.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"papio/internal/cli"
	"papio/internal/pdf"
	"papio/internal/protocol"
)

func main() {
	if filepath.Base(os.Args[0]) == "papio-native-host" {
		// Chrome invokes the native host with the extension origin as the first
		// argument. The bridge lands in Phase 2; stdout remains frame-only.
		fmt.Fprintln(os.Stderr, "papio-native-host: bridge not implemented yet (Phase 2); protocol "+protocol.BrowserProtocolVersion)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == pdf.WorkerArgument {
		if err := pdf.WorkerMain(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "pdf worker:", err)
			os.Exit(1)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root := cli.NewRoot(os.Stdout, os.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "papio:", err)
		os.Exit(1)
	}
}
