// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"papio/internal/api"
	"papio/internal/bench"
)

// Result is one cohort work's measured outcome.
type Result struct {
	Key      string              `json:"key"`
	Request  string              `json:"request"`
	Expected bench.ExpectedClass `json:"expected_class"`
	Outcome  Outcome             `json:"outcome"`
	Verdict  Verdict             `json:"verdict"`
	// StopDetail names WHY the job stopped where it did — the open action
	// kinds, the terminal reason, or the last transition reason. A report
	// without it counts the same wall today's `papio status` prints.
	StopDetail     string  `json:"stop_detail,omitempty"`
	JobID          string  `json:"job_id,omitempty"`
	FinalState     string  `json:"final_state,omitempty"`
	TerminalReason string  `json:"terminal_reason,omitempty"`
	ElapsedSeconds int64   `json:"elapsed_seconds"`
	SpentUSD       float64 `json:"spent_usd"`
	// UntriedCandidates is nil when no inspector ran. A pointer, not a zero
	// int, because "papio had none left" and "nobody looked" are opposite
	// findings and an int cannot tell them apart.
	UntriedCandidates *int   `json:"untried_candidates,omitempty"`
	TotalCandidates   *int   `json:"total_candidates,omitempty"`
	CandidateNote     string `json:"candidate_note,omitempty"`
	CreatedByRun      bool   `json:"created_by_run"`
	CleanedUp         bool   `json:"cleaned_up,omitempty"`
	CleanupNote       string `json:"cleanup_note,omitempty"`

	submittedAt time.Time
	// parkedSince is when this job was FIRST seen in a parking state, so a
	// parked sighting can be confirmed before it is recorded. Zeroed again
	// if the job leaves that state, which is the case this field exists for.
	parkedSince time.Time
}

// finish records a settled job on the result.
func (r *Result) finish(at time.Time, outcome Outcome, detail *api.JobDetailV3) {
	r.Outcome = outcome
	r.StopDetail = stopDetail(detail, outcome)
	r.ElapsedSeconds = int64(at.Sub(r.submittedAt).Seconds())
	if detail != nil && detail.Job != nil {
		r.FinalState = detail.Job.State
		r.TerminalReason = detail.Job.TerminalReason
		r.SpentUSD = detail.Job.SpentUSD
	}
}

// Skip is a cohort work the run did not measure, and why. Every skip is
// reported: it changes what each rate below is a rate of.
type Skip struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
	JobID  string `json:"job_id,omitempty"`
}

// Bucket is one label and its count, for the ordered tallies the report
// renders. Go map iteration is randomised and two runs of one cohort must
// render comparably.
type Bucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Report is one complete live-cohort measurement.
type Report struct {
	CohortID      string `json:"cohort_id"`
	RunID         string `json:"run_id"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	PerWorkBudget string `json:"per_work_budget"`
	// ParkSettle records the window a parked observation was confirmed over,
	// because it bounds how wrong the human-boundary counts below can be.
	ParkSettle string `json:"park_settle"`

	Results   []Result `json:"results"`
	Skipped   []Skip   `json:"skipped,omitempty"`
	Cancelled []string `json:"cancelled_job_ids,omitempty"`
	// Kept names the jobs this run created that produced an artifact and so
	// were spared. They are the operator's to keep or discard; the report
	// states them so the decision is theirs and not this tool's silence.
	Kept []string `json:"kept_job_ids,omitempty"`

	// Measured is the number of works that actually ran — the denominator
	// of every rate below.
	Measured int `json:"measured"`
	// WrongAccepts is the primary, unconditional gate.
	WrongAccepts int `json:"wrong_accepts"`
	// AutonomousReady is the throughput number, compared only after wrong
	// accepts are flat or down.
	AutonomousReady int `json:"autonomous_ready"`
	Met             int `json:"met"`

	Outcomes    []Bucket `json:"outcomes"`
	StopReasons []Bucket `json:"stop_reasons,omitempty"`
	// GaveUpHolding counts works that stopped for a human while at least one
	// fetch candidate was still untried. It is the direct measure of papio
	// asking a person for something it had not finished trying itself.
	GaveUpHolding int `json:"gave_up_holding_candidates"`
	// CandidatesInspected is how many results carry an untried count, so a
	// reader can see what GaveUpHolding is a count out of.
	CandidatesInspected int     `json:"candidates_inspected"`
	TotalSpentUSD       float64 `json:"total_spent_usd"`
}

// summarize computes every aggregate from Results. It runs once, at the end
// of Run, so the rendered report and the JSON report can never disagree.
func (r *Report) summarize() {
	outcomes := map[string]int{}
	stops := map[string]int{}
	r.Measured = len(r.Results)
	for i := range r.Results {
		row := r.Results[i]
		outcomes[string(row.Outcome)]++
		if row.StopDetail != "" && !row.Outcome.ready() {
			stops[string(row.Outcome)+": "+row.StopDetail]++
		}
		switch row.Verdict {
		case VerdictWrongAccept:
			r.WrongAccepts++
		case VerdictMet:
			r.Met++
		}
		if row.Outcome == AutonomousReady {
			r.AutonomousReady++
		}
		r.TotalSpentUSD += row.SpentUSD
		if row.UntriedCandidates != nil {
			r.CandidatesInspected++
			if *row.UntriedCandidates > 0 && askedAHuman(row.Outcome) {
				r.GaveUpHolding++
			}
		}
	}
	r.Outcomes = sortedBuckets(outcomes)
	r.StopReasons = sortedBuckets(stops)
}

// askedAHuman reports whether an outcome stopped on a person.
func askedAHuman(o Outcome) bool {
	return o == HumanBoundary || o == IdentityReview || o == AssistedReady
}

func sortedBuckets(counts map[string]int) []Bucket {
	buckets := make([]Bucket, 0, len(counts))
	for label, count := range counts {
		buckets = append(buckets, Bucket{Label: label, Count: count})
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Count != buckets[j].Count {
			return buckets[i].Count > buckets[j].Count
		}
		return buckets[i].Label < buckets[j].Label
	})
	return buckets
}

// Render writes the human-readable report.
//
// Wrong accepts print first and alone, above the throughput number, because
// the order the two numbers are read in is the whole discipline: a run that
// raises wrong accepts does not ship whatever it did to throughput.
func (r Report) Render(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "live-cohort %s  run %s\n", r.CohortID, r.RunID)
	fmt.Fprintf(&b, "started %s  finished %s\n", r.StartedAt, r.FinishedAt)
	fmt.Fprintf(&b, "per-work budget %s   parked observations confirmed over %s\n\n", r.PerWorkBudget, r.ParkSettle)

	fmt.Fprintf(&b, "WRONG ACCEPTS      %d   (primary gate — an increase does not ship)\n", r.WrongAccepts)
	fmt.Fprintf(&b, "autonomous ready   %d of %d   %s\n", r.AutonomousReady, r.Measured, percent(r.AutonomousReady, r.Measured))
	fmt.Fprintf(&b, "expectation met    %d of %d   %s\n", r.Met, r.Measured, percent(r.Met, r.Measured))
	if r.CandidatesInspected > 0 {
		fmt.Fprintf(&b, "asked a human while holding an untried candidate   %d of %d inspected\n",
			r.GaveUpHolding, r.CandidatesInspected)
	} else {
		fmt.Fprintf(&b, "asked a human while holding an untried candidate   unfilled (no store inspector)\n")
	}
	fmt.Fprintf(&b, "spend              $%.4f\n\n", r.TotalSpentUSD)

	b.WriteString("OUTCOMES\n")
	for _, bucket := range r.Outcomes {
		fmt.Fprintf(&b, "  %-20s %d\n", bucket.Label, bucket.Count)
	}

	if len(r.StopReasons) > 0 {
		b.WriteString("\nWHY THEY STOPPED\n")
		for _, bucket := range r.StopReasons {
			fmt.Fprintf(&b, "  %3d  %s\n", bucket.Count, bucket.Label)
		}
	}

	b.WriteString("\nPER WORK\n")
	fmt.Fprintf(&b, "  %-22s %-26s %-18s %-12s %6s  %s\n", "KEY", "EXPECTED", "OBSERVED", "VERDICT", "SECS", "DETAIL")
	for _, row := range r.Results {
		detail := row.StopDetail
		if row.UntriedCandidates != nil && *row.UntriedCandidates > 0 {
			detail += fmt.Sprintf(" [%d/%d candidates untried]", *row.UntriedCandidates, deref(row.TotalCandidates))
		}
		fmt.Fprintf(&b, "  %-22s %-26s %-18s %-12s %6d  %s\n",
			truncate(row.Key, 22), row.Expected, row.Outcome, row.Verdict, row.ElapsedSeconds, detail)
	}

	if len(r.Skipped) > 0 {
		b.WriteString("\nSKIPPED (not measured; every rate above excludes these)\n")
		for _, skip := range r.Skipped {
			fmt.Fprintf(&b, "  %-22s %s\n", truncate(skip.Key, 22), skip.Reason)
		}
	}
	if len(r.Cancelled) > 0 {
		fmt.Fprintf(&b, "\ncleanup: cancelled %d job(s) this run created and that produced no artifact\n", len(r.Cancelled))
	}
	if len(r.Kept) > 0 {
		fmt.Fprintf(&b, "cleanup: KEPT %d job(s) that acquired a paper — they are terminal, so cleanup\n", len(r.Kept))
		fmt.Fprintf(&b, "         cannot remove them and `papio jobs cancel` refuses them. They now sit in\n")
		fmt.Fprintf(&b, "         your ready queue; review with `papio jobs list --state ready`.\n")
		fmt.Fprintf(&b, "         %s\n", strings.Join(r.Kept, " "))
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func deref(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func percent(n, total int) string {
	if total == 0 {
		return "n/a"
	}
	return strconv.FormatFloat(100*float64(n)/float64(total), 'f', 1, 64) + "%"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
