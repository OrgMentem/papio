// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/bench"
)

// errBenchWorksFailed is the exit status of a run in which some cohort work
// could not be run at all (WorkResult.Error: a broken fixture, an unscripted
// human action). The report still prints, but its headline counts such a work
// as zero on both sides, so a CI gate reading only the status would otherwise
// pass a benchmark that measured less than the cohort it was given.
var errBenchWorksFailed = errors.New("bench could not run every cohort work")

// newBenchCommand runs papio's hermetic comparative acquisition benchmark
// (dev/post-build-followups.md item 4). Like the local native-host and
// configuration commands it never talks to the daemon: it builds its own
// ephemeral, fixture-backed acquisition service in-process, so it needs no
// opt.call and no rpcMethods entry in commandClassification.
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
				err = printPage(opt, "results", report.Works, false)
			} else {
				err = renderBenchReport(opt, report)
			}
			if err != nil {
				return err
			}
			failed := 0
			for _, w := range report.Works {
				if w.Error != "" {
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("%w: %d of %d failed", errBenchWorksFailed, failed, len(report.Works))
			}
			return nil
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
