// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/discovery"
	"papio/internal/ipc"
)

func newSearchCommand(opt *options) *cobra.Command {
	var limit, yearFrom, yearTo int
	var oaOnly, newOnly bool
	var cites, citedBy, relatedTo, source string
	command := &cobra.Command{
		Use:         "search [query]",
		Short:       "Search configured discovery backends for scholarly works",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return err
			}
			if (len(args) == 0 || strings.TrimSpace(args[0]) == "") &&
				strings.TrimSpace(cites) == "" &&
				strings.TrimSpace(citedBy) == "" && strings.TrimSpace(relatedTo) == "" {
				return fmt.Errorf("query is required unless a citation snowball DOI is supplied")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) == 1 {
				query = strings.TrimSpace(args[0])
			}
			params := discovery.SearchParams{
				Query: query, Limit: limit, YearFrom: yearFrom, YearTo: yearTo, OAOnly: oaOnly,
				Cites: cites, CitedBy: citedBy, RelatedTo: relatedTo, Source: source,
			}
			var works []discovery.DiscoveredWork
			if err := opt.call(cmd.Context(), "discovery.search", params, &works); err != nil {
				return sourceRequiresCurrentDaemon(source, err)
			}
			if newOnly {
				works = newWorksOnly(works)
			}
			if opt.jsonOutput {
				return opt.printJSON(works)
			}
			for _, discovered := range works {
				if _, err := fmt.Fprintf(opt.out, "%d | %s | %s | %s | %s | %d citations%s\n",
					discovered.Work.Year,
					firstAuthor(discovered.Work.Authors),
					discovered.Work.Title,
					emptyMarker(discovered.Work.DOI),
					oaMarker(discovered.IsOA),
					discovered.CitedBy,
					ownedSuffix(discovered.Owned),
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
	flags.StringVar(&source, "source", "", "discovery backend: openalex or semanticscholar (default: all configured)")
	flags.BoolVar(&newOnly, "new-only", false, "omit works already in your library; filters after --limit and may return fewer results")
	flags.StringVar(&cites, "cites", "", "DOI to find papers citing it (forward citations; OpenAlex cites: filter)")
	flags.StringVar(&citedBy, "cited-by", "", "DOI to find papers it cites (backward references; OpenAlex cited_by: filter)")
	flags.StringVar(&relatedTo, "related-to", "", "DOI to find OpenAlex-related papers (related_to: filter)")
	return command
}

func sourceRequiresCurrentDaemon(source string, err error) error {
	if strings.TrimSpace(source) == "" {
		return err
	}
	var remoteErr *ipc.RemoteError
	if !errors.As(err, &remoteErr) || remoteErr.Code != "invalid_argument" ||
		!strings.Contains(remoteErr.Message, `unknown field "source"`) {
		return err
	}
	return fmt.Errorf("%w: --source requires a daemon running this papio version; run 'papio daemon stop' and retry", err)
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

func newWorksOnly(works []discovery.DiscoveredWork) []discovery.DiscoveredWork {
	filtered := make([]discovery.DiscoveredWork, 0, len(works))
	for _, discovered := range works {
		if !discovered.Owned {
			filtered = append(filtered, discovered)
		}
	}
	return filtered
}

func ownedSuffix(owned bool) string {
	if owned {
		return " [in library]"
	}
	return ""
}
