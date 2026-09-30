// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/api"
)

// errNoPageCaptured is the exit status of a capture that stored nothing. The
// daemon reports busy, timeout, not_permitted and nav_failed as routine
// outcomes rather than RPC errors, which is right for the API; the command
// still prints that outcome, and then fails so a script or agent that checks
// only the exit status does not go on to read a fixture that was never saved.
var errNoPageCaptured = errors.New("no page was captured")

// adapterCaptureSucceeded is the one outcome that names a stored page: the
// bridge rewrites a "captured" with no stored path into nav_failed, and the
// path check keeps that promise here as well.
func adapterCaptureSucceeded(result api.AdapterCaptureResult) bool {
	return result.Outcome == "captured" && result.Path != ""
}

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
				if err := opt.printJSON(result); err != nil {
					return err
				}
			} else if err := printAdapterCapture(opt, result); err != nil {
				return err
			}
			if !adapterCaptureSucceeded(result) {
				if result.Outcome == "not_permitted" {
					return fmt.Errorf("%w: %s — capture needs [captures] enabled = true in the config and a current extension listed by `papio browser sessions`", errNoPageCaptured, result.Outcome)
				}
				return fmt.Errorf("%w: %s", errNoPageCaptured, result.Outcome)
			}
			return nil
		},
	}
	command.Flags().StringVar(&provider, "provider", "", "provider adapter id")
	command.Flags().StringVar(&scenario, "scenario", "", "fixture scenario")
	command.Flags().Int64Var(&settleMS, "settle-ms", 0, "milliseconds to settle after page load (0-10000)")
	_ = command.MarkFlagRequired("provider")
	_ = command.MarkFlagRequired("scenario")
	return command
}

func printAdapterCapture(opt *options, result api.AdapterCaptureResult) error {
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
}
