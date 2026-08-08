// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"
)

func newGrabsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "grabs", Short: "Manage captured PDF grabs"}
	var doi, pmid, arxiv string
	identify := &cobra.Command{
		Use:   "identify <grab-id>",
		Short: "Bind an operator-supplied identifier to a captured PDF grab",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, value, err := grabIdentifierFlags(doi, pmid, arxiv)
			if err != nil {
				return err
			}
			var result api.GrabIdentifyResult
			if err := opt.call(cmd.Context(), "grabs.identify", map[string]string{
				"grab_id": args[0], "kind": kind, "value": value,
			}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if result.JobID != "" {
				_, err = fmt.Fprintf(opt.out, "%s\t%s\t%s\n", result.GrabID, result.Outcome, result.JobID)
			} else {
				_, err = fmt.Fprintf(opt.out, "%s\t%s\n", result.GrabID, result.Outcome)
			}
			return err
		},
	}
	identify.Flags().StringVar(&doi, "doi", "", "identify by DOI")
	identify.Flags().StringVar(&pmid, "pmid", "", "identify by PubMed ID")
	identify.Flags().StringVar(&arxiv, "arxiv", "", "identify by arXiv ID")
	command.AddCommand(identify)
	return command
}

func grabIdentifierFlags(doi, pmid, arxiv string) (string, string, error) {
	values := []struct{ kind, value string }{{"doi", doi}, {"pmid", pmid}, {"arxiv", arxiv}}
	chosen := 0
	var kind, value string
	for _, item := range values {
		if strings.TrimSpace(item.value) == "" {
			continue
		}
		chosen++
		kind, value = item.kind, item.value
	}
	if chosen != 1 {
		return "", "", errors.New("exactly one of --doi, --pmid, or --arxiv is required")
	}
	return kind, value, nil
}
