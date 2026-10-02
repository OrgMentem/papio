// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"

	"github.com/spf13/cobra"
	"papio/internal/job"
)

func addArchiveCommands(parent *cobra.Command, opt *options) {
	for _, operation := range []string{"archive", "restore", "disposition"} {
		operation := operation
		var confirmed bool
		command := &cobra.Command{
			Use:   operation + " <job-id>",
			Short: map[string]string{"archive": "Archive a validated acquisition without deleting its artifact", "restore": "Restore an archived acquisition to the actionable ready view", "disposition": "Inspect acquisition disposition and retained artifact"}[operation],
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if operation == "archive" && !confirmed {
					return errors.New("--confirm is required; archive retains the artifact and blocks automatic consumption")
				}
				var result job.DispositionResult
				if err := opt.call(cmd.Context(), "jobs."+operation, map[string]any{"job_id": args[0], "confirmed": confirmed}, &result); err != nil {
					if isUnknownMethod(err) {
						return daemonUpgradeRequired("jobs." + operation)
					}
					return err
				}
				return opt.printResult(result, "%s: %s (state %s, artifact %s)", result.JobID, result.Disposition, result.State, result.ArtifactSHA256)
			},
		}
		if operation == "archive" {
			command.Flags().BoolVar(&confirmed, "confirm", false, "confirm the archive decision; do not delete retained artifacts")
		}
		if operation == "disposition" {
			command.Annotations = map[string]string{"mcp:read-only": "true"}
		}
		parent.AddCommand(command)
	}
}
