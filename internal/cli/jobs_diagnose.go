// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/store"
)

func newJobsDiagnoseCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "diagnose <job-id>",
		Short:       "Explain why one job needs attention and what can happen next",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var diagnosis api.JobDiagnosis
			if err := opt.call(cmd.Context(), "jobs.diagnose_v1", map[string]string{"job_id": args[0]}, &diagnosis); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("jobs.diagnose_v1")
				}
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(diagnosis)
			}
			return printJobDiagnosis(opt, diagnosis)
		},
	}
}

func printJobDiagnosis(opt *options, diagnosis api.JobDiagnosis) error {
	if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\n", diagnosis.JobID, diagnosis.State, diagnosis.Reason); err != nil {
		return err
	}
	if diagnosis.Work != "" {
		if _, err := fmt.Fprintf(opt.out, "work\t%s\n", store.StripTerminalControls(diagnosis.Work)); err != nil {
			return err
		}
	}
	if diagnosis.Title != "" {
		if _, err := fmt.Fprintf(opt.out, "title\t%s\n", store.StripTerminalControls(diagnosis.Title)); err != nil {
			return err
		}
	}
	if diagnosis.ProviderOutcome != "" {
		if _, err := fmt.Fprintf(opt.out, "provider outcome\t%s\n", diagnosis.ProviderOutcome); err != nil {
			return err
		}
	}
	if diagnosis.AdapterID != "" {
		adapter := diagnosis.AdapterID
		if diagnosis.AdapterVersion != "" {
			adapter += "@" + diagnosis.AdapterVersion
		}
		if _, err := fmt.Fprintf(opt.out, "adapter\t%s\n", adapter); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(opt.out, "why\t%s\nnext\t%s\n", diagnosis.Why, diagnosis.Next); err != nil {
		return err
	}
	if diagnosis.Action != nil {
		if _, err := fmt.Fprintf(opt.out, "action\t%d\t%s\t%s\n", diagnosis.Action.ActionID, diagnosis.Action.Kind, diagnosis.Action.Status); err != nil {
			return err
		}
		if diagnosis.Action.CanOpenAction {
			if _, err := fmt.Fprintf(opt.out, "open\tpapio actions open --job %s\n", diagnosis.JobID); err != nil {
				return err
			}
		}
	}
	capabilities := make([]string, 0, 2)
	if diagnosis.CanRetry {
		capabilities = append(capabilities, "papio jobs retry "+diagnosis.JobID)
	}
	if diagnosis.NeedsBrowser {
		capabilities = append(capabilities, "browser required")
	}
	if len(capabilities) != 0 {
		_, err := fmt.Fprintf(opt.out, "capabilities\t%s\n", strings.Join(capabilities, ", "))
		return err
	}
	return nil
}
