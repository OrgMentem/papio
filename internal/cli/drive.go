// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"github.com/spf13/cobra"

	"papio/internal/drive"
)

// newDriveCommand is the operator surface of the paced drive: the daemon
// pass that opens parked handoffs one at a time when [drive] enabled = true.
//
// `drive resume` is hidden from the MCP facade. Resuming lets papio open
// human work with nobody asking, which is the operator's decision (ADR-0009,
// amended 2026-09-23); a prompt-injected agent must not be able to make it.
// Pausing only stops opens, so it stays reachable.
func newDriveCommand(opt *options) *cobra.Command {
	command := &cobra.Command{
		Use:   "drive",
		Short: "Inspect and pause the paced drive of parked handoffs",
		Long: "The paced drive opens parked handoffs for you, one at a time, oldest first,\n" +
			"through the same path as 'papio actions open'. It is off unless [drive]\n" +
			"enabled = true in the config, and it waits whenever the browser is busy,\n" +
			"a sign-in is open, a provider is refusing this browser, or the hourly\n" +
			"bound is spent. A sign-in nobody completes pauses it and notifies you.",
	}
	run := func(method, prose string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			var status drive.Status
			if err := opt.call(cmd.Context(), method, map[string]any{}, &status); err != nil {
				return err
			}
			if prose == "" {
				return opt.printResult(status, "%s", status.String())
			}
			return opt.printResult(status, "%s\n%s", prose, status.String())
		}
	}
	status := &cobra.Command{
		Use:         "status",
		Short:       "Show whether the paced drive would open a handoff, and what blocks it",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE:        run("drive.status", ""),
	}
	pause := &cobra.Command{
		Use:   "pause",
		Short: "Stop paced opens until 'papio drive resume'",
		Args:  cobra.NoArgs,
		RunE:  run("drive.pause", "paced drive paused"),
	}
	resume := &cobra.Command{
		Use:         "resume",
		Short:       "Let the paced drive open handoffs again",
		Annotations: map[string]string{"mcp:hidden": "true"},
		Args:        cobra.NoArgs,
		RunE:        run("drive.resume", "paced drive resumed"),
	}
	command.AddCommand(status, pause, resume)
	return command
}
