// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/job"
)

// actionSelector narrows the handoff queue to the row a caller named.
//
// It exists because `actions open` used to be head-of-queue only, so a consumer
// that had ranked the queue itself — by citation reach, by cohort, by whatever
// it actually cares about — could not open the row it chose. The important half
// is the failure mode: a selector that matches nothing must be an error, never a
// silent fall back to the whole queue, or the caller opens an unrelated
// institution's handoff and is told it succeeded.
type actionSelector struct {
	jobID    string
	actionID int64
}

func newActionSelector(cmd *cobra.Command, jobID string, actionID int64) (actionSelector, error) {
	byJob := cmd.Flags().Changed("job")
	byAction := cmd.Flags().Changed("action")
	switch {
	case byJob && byAction:
		return actionSelector{}, errors.New("use either --job or --action, not both")
	case byJob && jobID == "":
		return actionSelector{}, errors.New("--job requires a job id")
	case byAction && actionID <= 0:
		return actionSelector{}, errors.New("--action requires a positive action id")
	}
	return actionSelector{jobID: jobID, actionID: actionID}, nil
}

func (s actionSelector) active() bool { return s.jobID != "" || s.actionID > 0 }

// apply keeps only the selected rows, preserving the caller's newest-first
// order. An unmatched selector reports what it looked for rather than how many
// rows it rejected: the caller already knows the id it passed, and needs to know
// whether the row is gone, resolved, or never existed.
func (s actionSelector) apply(actions []job.HumanAction) ([]job.HumanAction, error) {
	if !s.active() {
		return actions, nil
	}
	out := make([]job.HumanAction, 0, 1)
	for _, action := range actions {
		if s.actionID > 0 && action.ID != s.actionID {
			continue
		}
		if s.jobID != "" && action.JobID != s.jobID {
			continue
		}
		out = append(out, action)
	}
	if len(out) == 0 {
		if s.actionID > 0 {
			return nil, fmt.Errorf("no open human action with id %d; it may have been resolved or dismissed — check 'papio actions list --all'", s.actionID)
		}
		return nil, fmt.Errorf("no open human action for job %q; it may have been resolved or the job may have moved on — check 'papio jobs get %s'", s.jobID, s.jobID)
	}
	return out, nil
}

// describe names the rows a "nothing openable" report is about.
func (s actionSelector) describe(count int) string {
	switch {
	case s.actionID > 0:
		return fmt.Sprintf("action %d", s.actionID)
	case s.jobID != "":
		return fmt.Sprintf("%d open action(s) on job %s", count, s.jobID)
	default:
		return fmt.Sprintf("%d open action(s)", count)
	}
}

// consumerHint renders attribution as its own column, empty when the submission
// recorded none — an unattributed row shows nothing rather than a placeholder.
func consumerHint(consumer *string) string {
	if consumer == nil || *consumer == "" {
		return ""
	}
	return "\t" + *consumer
}

// staleHint names how long a stale action has been waiting. Only stale rows say
// anything: an age on every row buries the handful that matter, and the age
// itself is already in --json for anyone counting.
func staleHint(action api.ActionRow) string {
	if !action.Stale {
		return ""
	}
	return "\tstale: waiting " + waitingFor(time.Duration(action.AgeSeconds)*time.Second)
}

// waitingFor renders an age at the coarsest unit that still says something. It
// falls through to seconds rather than rounding down to "0m", which is what a
// short configured threshold produced and which reads like a broken counter
// rather than a young action.
func waitingFor(age time.Duration) string {
	switch {
	case age >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	case age >= time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	case age >= time.Minute:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(age/time.Second))
	}
}
