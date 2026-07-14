// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/discovery"
)

func newSearchCommand(opt *options) *cobra.Command {
	var limit, yearFrom, yearTo int
	var oaOnly bool
	command := &cobra.Command{
		Use:   "search <query>",
		Short: "Search OpenAlex for scholarly works",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := discovery.SearchParams{
				Query: strings.TrimSpace(args[0]), Limit: limit, YearFrom: yearFrom, YearTo: yearTo, OAOnly: oaOnly,
			}
			var works []discovery.DiscoveredWork
			if err := opt.call(cmd.Context(), "discovery.search", params, &works); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(works)
			}
			for _, discovered := range works {
				if _, err := fmt.Fprintf(opt.out, "%d | %s | %s | %s | %s | %d citations\n",
					discovered.Work.Year,
					firstAuthor(discovered.Work.Authors),
					discovered.Work.Title,
					emptyMarker(discovered.Work.DOI),
					oaMarker(discovered.IsOA),
					discovered.CitedBy,
				); err != nil {
					return err
				}
			}
			return nil
		},
	}
	flags := command.Flags()
	flags.IntVar(&limit, "limit", 20, "maximum results (1-50)")
	flags.IntVar(&yearFrom, "year-from", 0, "minimum publication year")
	flags.IntVar(&yearTo, "year-to", 0, "maximum publication year")
	flags.BoolVar(&oaOnly, "oa-only", false, "return only open-access works")
	return command
}

func firstAuthor(authors []string) string {
	if len(authors) == 0 || strings.TrimSpace(authors[0]) == "" {
		return "—"
	}
	return authors[0]
}

func emptyMarker(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func oaMarker(isOA bool) string {
	if isOA {
		return "OA"
	}
	return "—"
}
