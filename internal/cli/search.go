// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/agentjson"
	"papio/internal/discovery"
	"papio/internal/ipc"
	"papio/internal/store"
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
			effective := effectiveLimitFloored(limit, discovery.MaxLimit, discovery.DefaultLimit)
			params := discovery.SearchParams{
				Query: query, Limit: effective, YearFrom: yearFrom, YearTo: yearTo, OAOnly: oaOnly,
				Cites: cites, CitedBy: citedBy, RelatedTo: relatedTo, Source: source,
			}
			var works []discovery.DiscoveredWork
			if err := opt.call(cmd.Context(), "discovery.search", params, &works); err != nil {
				return sourceRequiresCurrentDaemon(source, err)
			}
			// truncated reflects the pre-filter fetched page against the
			// daemon's own effective limit: --new-only removing owned rows
			// below must not turn a capped page into a falsely-complete one.
			_, truncated := agentjson.Capped(works, effective)
			if newOnly {
				works = newWorksOnly(works)
			}
			if opt.jsonOutput {
				return printPage(opt, "works", works, truncated)
			}
			var anyConfident, anyTitleJudged bool
			for _, discovered := range works {
				// discovered.Work.Title/Authors are third-party bibliographic
				// metadata: internal/discovery normalizes them with only
				// strings.TrimSpace before returning a hit (discovery.go:451,
				// semanticscholar.go:347). discovered.Work.DOI goes through
				// work.NormalizeDOI instead, which does NOT strip control bytes
				// either — doiCoreRE's \S excludes only [\t\n\f\r ] in RE2, so
				// ESC/BEL/DEL/C1 all survive (same gap the inbox retraction row
				// closes). Anyone who can register a work in OpenAlex or
				// Semantic Scholar controls all three bytes verbatim, and papio
				// search is the widest-reach surface — any keyword query can
				// surface an attacker-registered row. Route them through the
				// same StripTerminalControls choke point ActivityText/watch
				// digest use, or a poisoned title/author/DOI injects ANSI/OSC
				// escapes here.
				if _, err := fmt.Fprintf(opt.out, "%d | %s | %s | %s | %s | %s | %d citations%s\n",
					discovered.Work.Year,
					firstAuthor(discovered.Work.Authors),
					store.StripTerminalControls(discovered.Work.Title),
					emptyMarker(discovered.Work.DOI),
					oaMarker(discovered.IsOA),
					matchMarker(discovered.MatchKind),
					discovered.CitedBy,
					ownedSuffix(discovered.Owned),
				); err != nil {
					return err
				}
				switch {
				case discovery.Confident(discovered.MatchKind):
					anyConfident = true
				case discovered.MatchKind != discovery.MatchUnscored:
					anyTitleJudged = true
				}
			}
			// A free-text query whose title got judged (at least one row scored
			// beyond unscored) but never reached a confident kind is the L3 field
			// report failure mode: the tool found rows and returned them without
			// signaling that none of them look like the title asked for. A
			// citation snowball or a short keyword query never sets
			// anyTitleJudged (every row stays MatchUnscored), so this stays quiet
			// for the searches where "no confident title" is not a meaningful
			// statement.
			if query != "" && !anyConfident && anyTitleJudged {
				if _, err := fmt.Fprintf(opt.out, "no confident title match for %q — showing the closest results anyway\n", query); err != nil {
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

// firstAuthor is only ever called on discovery-backend author data (see the
// comment above the search row printf), so stripping here rather than at
// the call site keeps a future caller safe by construction.
func firstAuthor(authors []string) string {
	if len(authors) == 0 || strings.TrimSpace(authors[0]) == "" {
		return "—"
	}
	return store.StripTerminalControls(authors[0])
}

// emptyMarker is only ever called on discovery-backend DOI data (see the
// comment above the search row printf); stripping here rather than at the
// call site keeps a future caller safe by construction.
func emptyMarker(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return store.StripTerminalControls(value)
}

func oaMarker(isOA bool) string {
	if isOA {
		return "OA"
	}
	return "—"
}

// matchMarker renders why a row ranked where it did. unscored (the default)
// stays as quiet as emptyMarker's placeholder: keyword and citation-snowball
// searches leave every row unscored, and a loud marker on every line of an
// ordinary search would be noise. See discovery.Confident for the policy.
func matchMarker(kind string) string {
	switch kind {
	case discovery.MatchExactTitle:
		return "EXACT"
	case discovery.MatchTitlePhrase:
		return "PHRASE"
	case discovery.MatchTitleTokens:
		return "TOKENS"
	case discovery.MatchWeak:
		return "WEAK"
	default:
		return "—"
	}
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
