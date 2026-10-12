// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/api"
)

// errClosureUnconfirmed refuses the acknowledgement without --confirm: it
// reopens paid egress, so it never runs on a bare invocation.
var errClosureUnconfirmed = errors.New("acknowledging a closure reopens paid egress for the source, spending your provider credits again; re-run with --confirm to proceed")

// newAcknowledgeClosureCommand is the operator's way to end a sticky egress
// closure (ADR-0024 §8). It is hidden from the MCP facade: reopening spending
// is the operator's decision, and a prompt-injected agent must not be able to
// make it. The daemon method refuses an MCP caller as well.
func newAcknowledgeClosureCommand(opt *options) *cobra.Command {
	var confirm bool
	command := &cobra.Command{
		Use:   "acknowledge-closure SOURCE",
		Short: "Reopen paid egress for a source that closed on price or balance drift (requires --confirm)",
		Long: "A source's paid egress closes, and stays closed with no timed reopen, when the\n" +
			"provider's prices stop matching the schedule papio committed against or when\n" +
			"its prepaid balance falls below the starting observation. 'papio doctor' names\n" +
			"the closed source and the reason.\n\n" +
			"This command acknowledges the closure after you have checked the provider's\n" +
			"account, and reopens egress. The next response from the provider sets the new\n" +
			"balance baseline. Without --confirm nothing changes. It exits nonzero when the\n" +
			"source was not closed.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:hidden": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return errClosureUnconfirmed
			}
			var result api.AcknowledgeClosureResult
			if err := opt.call(cmd.Context(), "budget.acknowledge_closure_v1", api.AcknowledgeClosureParams{Source: args[0]}, &result); err != nil {
				return err
			}
			if !result.Cleared {
				if opt.jsonOutput {
					if err := opt.printJSON(result); err != nil {
						return err
					}
				}
				return fmt.Errorf("%s egress is not closed; nothing to acknowledge", result.Source)
			}
			return opt.printResult(result, "%s egress closure acknowledged; egress is open again", result.Source)
		},
	}
	command.Flags().BoolVar(&confirm, "confirm", false, "reopen paid egress for the source")
	return command
}
