// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strings"

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
			if !adapterRepairSegmentRE.MatchString(provider) || len(provider) > 64 {
				return fmt.Errorf("--provider %q must be an adapter id such as wiley or sage (lowercase, starting with a letter), or `adapter repair` cannot use the capture", provider)
			}
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
					return fmt.Errorf("%w: %s%s", errNoPageCaptured, result.Outcome, notPermittedRemedy(result.Detail))
				}
				return fmt.Errorf("%w: %s", errNoPageCaptured, result.Outcome)
			}
			return nil
		},
	}
	command.Flags().StringVar(&provider, "provider", "", "provider adapter id: lowercase, such as wiley or sage")
	command.Flags().StringVar(&scenario, "scenario", "", "fixture scenario")
	command.Flags().Int64Var(&settleMS, "settle-ms", 0, "milliseconds to settle after page load (0-10000)")
	_ = command.MarkFlagRequired("provider")
	_ = command.MarkFlagRequired("scenario")
	return command
}

// notPermittedRemedy picks the fix from the daemon's detail. The outcome has
// several unrelated causes, and only the first one is a config edit.
func notPermittedRemedy(detail string) string {
	if detail == "" {
		return ""
	}
	lower := strings.ToLower(detail)
	var remedy string
	switch {
	case strings.Contains(lower, "storage is disabled"):
		remedy = "set `[captures] enabled = true` in the config and restart the daemon"
	case strings.Contains(lower, "host permission"):
		remedy = "grant the provider host in the papio extension options page; the browser only allows that grant through its prompt"
	case strings.Contains(lower, "consent"):
		remedy = "enable page capture consent in the papio extension settings"
	case strings.Contains(lower, "cannot correlate"):
		remedy = "update the papio extension, then run `papio browser sessions` to confirm its version"
	case strings.Contains(lower, "no compatible browser session"):
		remedy = "connect a current papio extension and confirm it with `papio browser sessions`"
	}
	if remedy == "" {
		return " (" + detail + ")"
	}
	return " (" + detail + "): " + remedy
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
