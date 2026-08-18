// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

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
	command.AddCommand(newGrabsBindsCommand(opt))
	command.AddCommand(newGrabsSuggestCommand(opt))
	command.AddCommand(newGrabsConfirmCommand(opt))
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

// grabsBindsResult decodes the grabs.binds envelope. There is no api-level
// wrapper struct for the two-key envelope itself (internal/api builds it
// straight from internal/agentjson.Envelope), but api.GrabBindRow is the
// daemon-owned row shape — reused here, same as GrabIdentifyResult above,
// so the CLI's view of a bind never drifts from the row the daemon wrote.
type grabsBindsResult struct {
	Binds     []api.GrabBindRow `json:"binds"`
	Truncated bool              `json:"truncated"`
}

// newGrabsBindsCommand lists captures papio filed on its own, without asking.
// Autonomous binding has no undo: `grabs identify` corrects a grab papio
// declined to bind, but nothing reverses one it already bound. Reviewing
// this list — which rule fired, which candidates it weighed, and the
// evidence that made one of them the winner — is the only recourse when an
// automatic filing turns out to be wrong.
func newGrabsBindsCommand(opt *options) *cobra.Command {
	var limit int
	command := &cobra.Command{
		Use:   "binds",
		Short: "List captures papio filed automatically, without asking",
		Long: "List captures papio bound to a pending job on its own, newest first.\n\n" +
			"papio only does this when a settled, DOI-less capture qualifies exactly one\n" +
			"pending job; everything else still parks for a human. Because there is no\n" +
			"unbind command, this listing — the rule version, how many candidates were on\n" +
			"the table, and the evidence that made one of them the winner — is the only\n" +
			"way to check an automatic filing after the fact.",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result grabsBindsResult
			if err := opt.call(cmd.Context(), "grabs.binds", map[string]any{"limit": limit}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("grabs.binds")
				}
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "binds", result.Binds, result.Truncated)
			}
			for _, bind := range result.Binds {
				evidence := strings.Join(bind.Provenance.Evidence, "; ")
				if evidence == "" {
					evidence = "(no evidence recorded)"
				}
				if _, err := fmt.Fprintf(opt.out, "%s | grab %s | job %s | %s | %d candidates | %s\n",
					bind.BoundAt.Format(time.RFC3339Nano),
					shortBindID(bind.GrabID), shortBindID(bind.JobID),
					bind.Provenance.Rule, bind.Provenance.CandidatesConsidered, evidence); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().IntVar(&limit, "limit", 50, "maximum autonomous binds to list (default 50, max 200)")
	return command
}

func shortBindID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// newGrabsSuggestCommand answers "which of my pending papers is this?" for a
// capture that parked instead of binding on its own. Autonomous binding only
// fires when exactly one pending job qualifies against production's
// QualifyCandidate/SelectAutoBindCandidate gates (unchanged, reused here);
// measured, that is about one capture in five — the rest need a human to
// pick. This command scores every pending job against the parked bytes with
// that same predicate and hands back the ranking, so the human is choosing
// from a list the daemon has already vetted rather than retyping an
// identifier it may already have read out of the file. It is read-only and
// therefore cannot misfile anything, which is exactly why fuzzy signals are
// allowed to inform the ORDER here even though they are barred from the
// acceptance gate itself.
func newGrabsSuggestCommand(opt *options) *cobra.Command {
	var limit int
	command := &cobra.Command{
		Use:   "suggest <grab-id>",
		Short: "Rank pending jobs against a parked capture: which paper is this?",
		Long: "Rank the pending jobs that best match a capture which parked instead of\n" +
			"binding on its own, most-likely first, with the evidence behind each\n" +
			"ranking. Nothing is filed by running this — it only orders the candidates\n" +
			"a human would otherwise have to hunt through by hand; pick one and run\n" +
			"`papio grabs confirm` to actually file it.\n\n" +
			"If the captured file states its own DOI, PMID, or arXiv ID in its front\n" +
			"matter, that identifier is printed first: `papio grabs identify` with that\n" +
			"value is faster and more certain than picking from the ranked list below it.",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.GrabSuggestResult
			if err := opt.call(cmd.Context(), "grabs.suggest", map[string]any{
				"grab_id": args[0], "limit": limit,
			}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("grabs.suggest")
				}
				return err
			}
			if opt.jsonOutput {
				// The envelope carries only the ranked list: document_identifiers
				// and outcome/detail are the human-workflow shortcut described
				// above, not something a scripted consumer of this row set needs —
				// a caller that wants them can read grab.Grab's own quarantined
				// bytes directly, the same source this command reads.
				return printPage(opt, "suggestions", result.Suggestions, result.Truncated)
			}
			return printGrabSuggestions(opt, result)
		},
	}
	command.Flags().IntVar(&limit, "limit", 5, "maximum ranked suggestions to return (default 5, max 25)")
	return command
}

// printGrabSuggestions renders grabs.suggest for a human. Document
// identifiers pulled straight from the file's own front matter print first
// and unconditionally, ahead of the ranked list, because that case has a
// strictly better next step — `papio grabs identify` with a value the
// operator no longer has to go find — and the ranked guesses below exist
// only for when the file does not say who it is.
func printGrabSuggestions(opt *options, result api.GrabSuggestResult) error {
	w := opt.out
	if result.Outcome != "ok" {
		detail := result.Detail
		if detail == "" {
			detail = result.Outcome
		}
		_, err := fmt.Fprintf(w, "%s: %s\n", result.GrabID, detail)
		return err
	}
	for _, id := range result.DocumentIdentifiers {
		if _, err := fmt.Fprintf(w, "file states %s %s (from %s) — run: papio grabs identify %s --%s %s\n",
			id.Kind, id.Value, id.Source, result.GrabID, id.Kind, id.Value); err != nil {
			return err
		}
	}
	if len(result.Suggestions) == 0 {
		_, err := fmt.Fprintln(w, "no pending jobs matched this capture")
		return err
	}
	for i, s := range result.Suggestions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		year := ""
		if s.Year != 0 {
			year = fmt.Sprintf(" (%d)", s.Year)
		}
		// Evidence is the reason a suggestion is a suggestion at all — joined
		// readably, never truncated, so the human has everything the daemon
		// weighed before they pick.
		evidence := strings.Join(s.Evidence, "; ")
		if evidence == "" {
			evidence = "(no evidence recorded)"
		}
		line := fmt.Sprintf("%d. [%s] %s%s — job %s", i+1, s.Verdict, title, year, s.JobID)
		if len(s.Authors) > 0 {
			line += " — " + strings.Join(s.Authors, ", ")
		}
		line += " — evidence: " + evidence
		if s.Reason != "" {
			line += " — reason: " + s.Reason
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	// The daemon already sorted qualifies-before-review-before-rejected, so
	// Suggestions[0] is the top pick; print the exact command that files it
	// so the operator can copy it instead of retyping the job id.
	_, err := fmt.Fprintf(w, "\npapio grabs confirm %s --job %s\n", result.GrabID, result.Suggestions[0].JobID)
	return err
}

// newGrabsConfirmCommand files a parked capture against the job a human
// picked — typically from the `papio grabs suggest` ranking, but any known
// job id works. It exists because ranking alone cannot file anything:
// SelectAutoBindCandidate already handles the one-qualifier case on its own,
// so a human only reaches this command when more than one job qualified or
// none did strongly enough for the daemon to decide by itself. The human's
// pick still loses to the document's own front matter — see refused_identity
// below — because extracted identity has outranked a human pick since
// autonomous binding shipped, and a manual confirm is not a carve-out from
// that rule.
func newGrabsConfirmCommand(opt *options) *cobra.Command {
	var jobID string
	command := &cobra.Command{
		Use:   "confirm <grab-id>",
		Short: "File a parked capture against the job you picked",
		Long: "Bind a parked capture to a specific pending job chosen by a human —\n" +
			"typically the top (or any) row of `papio grabs suggest`'s ranking.\n\n" +
			"papio still refuses the pick if the document's own front matter names a\n" +
			"different paper: extracted identity outranks a human pick, the same rule\n" +
			"autonomous binding already applies to itself. A refusal changes nothing —\n" +
			"no job is created and the capture stays parked exactly as it was before.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.GrabConfirmResult
			if err := opt.call(cmd.Context(), "grabs.confirm", map[string]string{
				"grab_id": args[0], "job_id": jobID,
			}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("grabs.confirm")
				}
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			return printGrabConfirmResult(opt, result)
		},
	}
	command.Flags().StringVar(&jobID, "job", "", "pending job id to file this capture against")
	_ = command.MarkFlagRequired("job")
	return command
}

// printGrabConfirmResult renders grabs.confirm for a human, with
// refused_identity singled out: it is the one outcome where the operator's
// explicit choice was overruled, so "nothing happened, and here is why" has
// to be unmistakable rather than folded into the same terse line every other
// outcome gets.
func printGrabConfirmResult(opt *options, result api.GrabConfirmResult) error {
	if result.Outcome == "refused_identity" {
		detail := result.Detail
		if detail == "" {
			detail = "the document's own front matter names a different paper"
		}
		_, err := fmt.Fprintf(opt.out, "%s: refused — %s; the pick was not applied, nothing changed\n", result.GrabID, detail)
		return err
	}
	if result.JobID != "" {
		_, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\n", result.GrabID, result.Outcome, result.JobID)
		return err
	}
	if result.Detail != "" {
		_, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\n", result.GrabID, result.Outcome, result.Detail)
		return err
	}
	_, err := fmt.Fprintf(opt.out, "%s\t%s\n", result.GrabID, result.Outcome)
	return err
}
