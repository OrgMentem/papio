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
