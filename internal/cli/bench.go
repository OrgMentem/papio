// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/bench"
)

// newBenchCommand runs papio's hermetic comparative acquisition benchmark
// (dev/post-build-followups.md item 4). Unlike every other command in this
// tree it never talks to the daemon: it builds its own ephemeral,
// fixture-backed acquisition service in-process, so it needs no opt.call
// and no rpcMethods entry in commandClassification.
func newBenchCommand(opt *options) *cobra.Command {
	var cohortPath, fixturesDir string
	cmd := &cobra.Command{
		Use:         "bench",
		Short:       "Run the hermetic comparative acquisition benchmark over a cohort file",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cohort, err := bench.LoadCohort(cohortPath)
			if err != nil {
				return err
			}
			dir := fixturesDir
			if dir == "" {
				dir = bench.FixturesDirFor(cohortPath)
			}
			report, err := bench.Run(cmd.Context(), cohort, bench.DirFixtureSet{Dir: dir})
			if err != nil {
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "results", report.Works, false)
			}
			return renderBenchReport(opt, report)
		},
	}
	cmd.Flags().StringVar(&cohortPath, "cohort", "", "path to a papio-bench-cohort/1 document")
	_ = cmd.MarkFlagRequired("cohort")
	cmd.Flags().StringVar(&fixturesDir, "fixtures", "", "fixture directory (default: the cohort file's sibling \"fixtures\" directory)")
	return cmd
}

func renderBenchReport(opt *options, report bench.Report) error {
	if _, err := fmt.Fprintln(opt.out, "WORK\tEXPECTED\tBASELINE\tCURRENT\tBASELINE SOURCE\tCURRENT SOURCE"); err != nil {
		return err
	}
	for _, w := range report.Works {
		if w.Error != "" {
			if _, err := fmt.Fprintf(opt.out, "%s\terror: %s\n", w.Key, w.Error); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\t%s\t%s\t%s\n",
			w.Key, w.ExpectedClass, w.BaselineClass, w.CurrentClass, w.BaselineAcceptedSource, w.CurrentAcceptedSource); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(opt.out, "\nincremental_autonomous_ready: %s\n", report.Headline()); err != nil {
		return err
	}
	_, err := fmt.Fprintln(opt.out, "note: the baseline overlay disables semanticscholar, openaire, and crossref_metadata; crossref_metadata has no independent typed-relations toggle, so disabling it also turns off title-only metadata enrichment.")
	return err
}
