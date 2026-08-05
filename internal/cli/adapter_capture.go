// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/api"
)

type adapterCaptureParams struct {
	URL      string `json:"url"`
	Provider string `json:"provider"`
	Scenario string `json:"scenario"`
	SettleMS *int64 `json:"settle_ms,omitempty"`
}

func newAdapterCaptureCommand(opt *options) *cobra.Command {
	var provider string
	var scenario string
	var settleMS int64
	command := &cobra.Command{
		Use:   "capture <url>",
		Short: "Capture a provider page through the connected browser",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := adapterCaptureParams{URL: args[0], Provider: provider, Scenario: scenario}
			if cmd.Flags().Changed("settle-ms") {
				params.SettleMS = &settleMS
			}
			var result api.AdapterCaptureResult
			if err := opt.call(cmd.Context(), "adapter.capture_v1", params, &result); err != nil {
				// Two papio binaries on one machine is documented as routine, so
				// a new CLI meeting an older daemon is an ordinary outcome, not
				// an exotic one. Every other versioned command renders the
				// actionable upgrade message here rather than a raw JSON-RPC
				// error.
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("adapter.capture_v1")
				}
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if result.Path != "" {
				_, err := fmt.Fprintf(opt.out, "%s\t%s\n", result.Outcome, result.Path)
				return err
			}
			if result.Detail != "" {
				_, err := fmt.Fprintf(opt.out, "%s\t%s\n", result.Outcome, result.Detail)
				return err
			}
			_, err := fmt.Fprintln(opt.out, result.Outcome)
			return err
		},
	}
	command.Flags().StringVar(&provider, "provider", "", "provider adapter id")
	command.Flags().StringVar(&scenario, "scenario", "", "fixture scenario")
	command.Flags().Int64Var(&settleMS, "settle-ms", 0, "milliseconds to settle after page load (0-10000)")
	_ = command.MarkFlagRequired("provider")
	_ = command.MarkFlagRequired("scenario")
	return command
}
