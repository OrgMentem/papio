// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/store"
	"papio/internal/zotio"
)

// newZotioTagsRetractionsCommand previews, and with --apply writes, the
// papio-owned retraction tag. It is hidden from MCP: it writes to the user's
// Zotero library and the preview-then-apply step is meant for a human, so an
// agent cannot reach the write through papio_command_run.
func newZotioTagsRetractionsCommand(opt *options) *cobra.Command {
	var apply bool
	command := &cobra.Command{
		Use:         "retractions",
		Short:       "Preview or apply the papio:retracted tag on items with a verified retraction notice",
		Long:        "Previews by default: lists the Zotero items whose DOI exactly matches a current verified retraction notice and the papio-owned tags no longer warranted. --apply writes the automatic papio:retracted tag and removes only papio-owned ones. Corrections and expressions of concern are never tagged, a manual tag is never changed, and acknowledging the inbox alert does not remove the tag. With zotio.retraction_tags off, --apply only removes papio-owned tags.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:hidden": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result zotio.RetractionTagResult
			if err := opt.call(cmd.Context(), "zotio.tags.retractions", map[string]any{"apply": apply}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			return writeRetractionTagResult(opt, result)
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "Write the changes instead of only previewing them")
	return command
}

func writeRetractionTagResult(opt *options, result zotio.RetractionTagResult) error {
	mode := "Preview"
	if result.Applied {
		mode = "Applied"
	}
	if _, err := fmt.Fprintf(opt.out, "%s: %d retracted DOI(s); %d item(s) to tag %s, %d papio-owned tag(s) to remove\n",
		mode, result.Retracted, len(result.Add), result.Tag, len(result.Remove)); err != nil {
		return err
	}
	if !result.Enabled {
		note := "zotio.retraction_tags is off: --apply only removes papio-owned tags"
		if _, err := fmt.Fprintln(opt.out, note); err != nil {
			return err
		}
	}
	for _, change := range result.Add {
		if _, err := fmt.Fprintf(opt.out, "  add    %s  %s\n", change.ItemKey, store.StripTerminalControls(change.DOI)); err != nil {
			return err
		}
	}
	for _, change := range result.Remove {
		if _, err := fmt.Fprintf(opt.out, "  remove %s\n", change.ItemKey); err != nil {
			return err
		}
	}
	if result.Applied {
		_, err := fmt.Fprintf(opt.out, "%d added, %d removed, %d left alone (same-name manual tag)\n", result.Added, result.Removed, result.Preserved)
		return err
	}
	if len(result.Add)+len(result.Remove) > 0 {
		_, err := fmt.Fprintln(opt.out, "Run again with --apply to write these changes.")
		return err
	}
	return nil
}
