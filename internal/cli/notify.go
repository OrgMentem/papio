// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/notify"
)

func newNotifyCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "notify", Short: "Inspect and test notification routing"}
	command.AddCommand(newNotifyShowCommand(opt), newNotifyPreviewCommand(opt), newNotifyTestCommand(opt))
	return command
}

func newNotifyShowCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use: "show", Short: "Show effective notification routing", Args: cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true", "mcp:envelope": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result api.NotifyShowResult
			if err := opt.call(cmd.Context(), "notify.show_v1", struct{}{}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				// The envelope is exactly {rows, truncated}, so the preset that
				// produced this routing rides on each row; dropping it left
				// --json unable to say which preset was in effect.
				format := localWebhookFormat(opt)
				rows := make([]notifyShowRow, len(result.Rows))
				for i, row := range result.Rows {
					rows[i] = notifyShowRow{NotifyRouteRow: row, Preset: result.Preset, WebhookFormat: format}
				}
				return printPage(opt, "rows", rows, false)
			}
			if _, err := fmt.Fprintf(opt.out, "preset: %s\n", result.Preset); err != nil {
				return err
			}
			if format := localWebhookFormat(opt); format != "" {
				if _, err := fmt.Fprintf(opt.out, "webhook_format: %s\n", format); err != nil {
					return err
				}
			}
			for _, row := range result.Rows {
				if _, err := fmt.Fprintf(opt.out, "%s | desktop=%s | webhook=%s | window=%ss | source=%s\n",
					row.Category, row.Desktop, row.Webhook, strconv.FormatInt(row.WindowSeconds, 10), row.Source); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// notifyShowRow is one `notify show --json` row: the daemon's route row plus
// the effective preset, which is the same on every row, and the webhook
// format from the CLI's own configuration when a webhook is configured.
type notifyShowRow struct {
	api.NotifyRouteRow
	Preset        string `json:"preset"`
	WebhookFormat string `json:"webhook_format,omitempty"`
}

// notifyPreviewOutput is the `notify preview --json` object: the daemon's
// result plus the webhook format from the CLI's own configuration.
type notifyPreviewOutput struct {
	api.NotifyPreviewResult
	WebhookFormat string `json:"webhook_format,omitempty"`
}

// localWebhookFormat reads the webhook format from the configuration this CLI
// loads, not from the daemon: daemon results are strict-decoded by older CLI
// binaries and cannot grow a field. It is empty when no webhook is configured
// or the configuration cannot be read. A running daemon keeps the format it
// started with until it restarts.
func localWebhookFormat(opt *options) string {
	cfg, err := opt.loadConfig()
	if err != nil || (cfg.Notify.WebhookURL == "" && cfg.Notify.WebhookCredentialRef == "") {
		return ""
	}
	return cfg.Notify.EffectiveWebhookFormat()
}

func newNotifyPreviewCommand(opt *options) *cobra.Command {
	var count int
	command := &cobra.Command{
		Use: "preview <category>", Short: "Preview notification copy without sending", Args: cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.NotifyPreviewResult
			if err := opt.call(cmd.Context(), "notify.preview_v1", api.NotifyPreviewParams{Category: args[0], Count: count}, &result); err != nil {
				return err
			}
			format := localWebhookFormat(opt)
			if opt.jsonOutput {
				return opt.printJSON(notifyPreviewOutput{NotifyPreviewResult: result, WebhookFormat: format})
			}
			if _, err := fmt.Fprintln(opt.out, result.Message); err != nil {
				return err
			}
			if format != "" {
				_, err := fmt.Fprintf(opt.out, "webhook_format: %s\n", format)
				return err
			}
			return nil
		},
	}
	command.Flags().IntVar(&count, "count", 1, "number of events represented in the preview")
	return command
}

func newNotifyTestCommand(opt *options) *cobra.Command {
	var webhook bool
	command := &cobra.Command{
		Use: "test <category>", Short: "Send one local or webhook notification test", Args: cobra.ExactArgs(1),
		Long:        "Send one test notification for a category. By default the test goes to the local desktop channel. With --webhook it is posted once to the configured webhook in its configured notify.webhook_format, bypasses the ledger, and reports whether the endpoint answered with a 2xx status.",
		Annotations: map[string]string{"mcp:side-effect": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if webhook {
				var result api.NotifyWebhookTestResult
				if err := opt.call(cmd.Context(), "notify.webhook_test_v1", api.NotifyWebhookTestParams{Category: args[0]}, &result); err != nil {
					return err
				}
				if opt.jsonOutput {
					return opt.printJSON(result)
				}
				_, err := fmt.Fprintf(opt.out, "%s\n%s\n", result.Message, result.Detail)
				return err
			}
			available, detail := notify.PlatformCapability()
			if !available {
				return fmt.Errorf("%s; use papio inbox or Activity", detail)
			}
			var result api.NotifyTestResult
			if err := opt.call(cmd.Context(), "notify.test_v1", api.NotifyTestParams{Category: args[0]}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if _, err := fmt.Fprintf(opt.out, "%s\n%s\n", result.Message, result.Detail); err != nil {
				return err
			}
			return nil
		},
	}
	command.Flags().BoolVar(&webhook, "webhook", false, "send the test through the configured webhook instead of the desktop")
	return command
}
