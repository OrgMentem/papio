// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/job"
)

// newJobsSupplyPDFCommand builds `papio jobs supply-pdf`. The command carries
// local path authority, so it is hidden from the MCP command facade: an agent
// that read a poisoned page must not be able to name a file on this machine.
func newJobsSupplyPDFCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "supply-pdf <job-id> <path>",
		Short: "Supply a local PDF as a job's main file",
		Long: "Supply a PDF you already have — an email attachment, an interlibrary loan copy, or an\n" +
			"earlier download — as the main file of an existing job. papio copies the file into its\n" +
			"data directory and runs the same checks as a browser download: the PDF must be readable\n" +
			"and must be the requested work. A PDF of a different work goes to identity review or is\n" +
			"rejected; it never becomes ready unchecked. Your original file is not changed.\n\n" +
			"The job must still be live: queued, resolving, fetching, or awaiting a human download.\n" +
			"Use add-component for supplements and appendices.",
		Annotations: map[string]string{"mcp:hidden": "true"},
		Args:        cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			staged, name, err := stageSuppliedPDF(cfg.DataDir, cfg.Fetch.MaxBytes, args[0], args[1])
			if err != nil {
				return err
			}
			// The daemon removes the staged copy once it has read or refused it.
			// This covers a call that never reached it.
			defer func() {
				_ = os.Remove(staged)
				_ = os.Remove(filepath.Dir(staged))
			}()
			var result api.SupplyPDFResult
			if err := opt.call(cmd.Context(), "jobs.supply_pdf", map[string]string{"job_id": args[0], "name": name}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("jobs.supply_pdf")
				}
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			_, err = fmt.Fprintf(opt.out, "%s\t%s\t%s\t%s\n", result.JobID, result.Outcome, result.State, result.SHA256)
			return err
		},
	}
}

// stageSuppliedPDF checks the operator's local file and copies it into the
// job's staging directory under the data directory. It returns the staged
// path and the file name the daemon is told; the daemon reads nothing else.
//
// The file must be a regular file (not a directory, device, or symlink), no
// larger than fetch.max_bytes — the bound on every PDF papio downloads — and
// must begin with the %PDF- header. These checks give the operator a clear
// refusal before any copy; the daemon checks confinement and size again, and
// its validation is what decides whether the PDF is the requested work.
func stageSuppliedPDF(dataDir string, limit int64, jobID, source string) (string, string, error) {
	stageDir, err := app.SuppliedPDFStagingDir(dataDir, jobID)
	if err != nil {
		return "", "", err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", "", err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "", "", fmt.Errorf("%s is a symbolic link; give the path of the PDF file itself", source)
	case info.IsDir():
		return "", "", fmt.Errorf("%s is a directory, not a PDF file", source)
	case !info.Mode().IsRegular():
		return "", "", fmt.Errorf("%s is not a regular file", source)
	case info.Size() > limit:
		return "", "", fmt.Errorf("%s is %d bytes, larger than the fetch.max_bytes limit of %d bytes", source, info.Size(), limit)
	}
	in, err := os.Open(source)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = in.Close() }()
	opened, err := in.Stat()
	if err != nil {
		return "", "", err
	}
	if !os.SameFile(info, opened) {
		return "", "", fmt.Errorf("%s changed while papio was reading it", source)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(in, header); err != nil || string(header) != "%PDF-" {
		return "", "", fmt.Errorf("%s is not a PDF file: it does not begin with %%PDF-", source)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return "", "", fmt.Errorf("creating the supply directory: %w", err)
	}
	name := job.NewID("supplied") + ".pdf"
	staged := filepath.Join(stageDir, name)
	out, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("staging the PDF: %w", err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(in, limit+1))
	closeErr := out.Close()
	if copyErr == nil && n > limit {
		copyErr = fmt.Errorf("%s grew past the fetch.max_bytes limit of %d bytes while papio was copying it", source, limit)
	}
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(staged)
		_ = os.Remove(stageDir)
		return "", "", fmt.Errorf("staging the PDF: %w", err)
	}
	return staged, name, nil
}
