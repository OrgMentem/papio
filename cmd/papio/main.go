// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio — legitimate paper-acquisition broker.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"papio/internal/agentcredential"
	"papio/internal/cli"
	"papio/internal/nativehost"
	"papio/internal/pdf"
)

func main() {
	if nativehost.InvokedAsHost(os.Args[0]) {
		// Chrome invokes the native host with the extension origin as an
		// untrusted argument. Stdout is frame-only; diagnostics go to stderr.
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := nativehost.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "papio-native-host:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == pdf.WorkerArgument {
		if err := pdf.WorkerMain(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "pdf worker:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == agentcredential.HelperArgument {
		if err := agentcredential.RunHelper(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "credential helper:", err)
			os.Exit(1)
		}
		return
	}
	// A hung OS credential call cannot be cancelled in process. Running each
	// call in a helper process lets the deadline kill it and free the gate.
	if executable, err := os.Executable(); err == nil {
		agentcredential.UseHelperProcess(executable)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if code := cli.RunProcess(ctx); code != 0 {
		cancel()
		os.Exit(code)
	}
}
