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
				return printPage(opt, "rows", result.Rows, false)
			}
			if _, err := fmt.Fprintf(opt.out, "preset: %s\n", result.Preset); err != nil {
				return err
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
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			_, err := fmt.Fprintln(opt.out, result.Message)
			return err
		},
	}
	command.Flags().IntVar(&count, "count", 1, "number of events represented in the preview")
	return command
}

func newNotifyTestCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use: "test <category>", Short: "Send one local notification test", Args: cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:side-effect": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
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
}
