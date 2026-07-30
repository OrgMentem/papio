// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"papio/internal/artifact"
	"papio/internal/job"
)

// ErrComponentRole reports a role this entry point will not accept.
var ErrComponentRole = errors.New("unsupported component role")

// The remaining sentinels exist so the RPC layer can tell an operator what to
// change. Every one of these is an ordinary, expected condition — the job has no
// main file yet, the path is outside the adoption root, the file is not a usable
// PDF — and collapsing them into a generic internal error, as an unclassified
// error does, leaves the caller nothing to act on. The wrapped text stays
// daemon-side for the log; the RPC layer emits a fixed safe message per sentinel
// because the confinement errors wrap filesystem errors carrying the user's path.
var (
	// ErrComponentPrecondition reports a job that cannot take a component yet.
	ErrComponentPrecondition = errors.New("job cannot take a component")
	// ErrComponentPath reports a source outside the job's adoption root, or one
	// that is not a regular file.
	ErrComponentPath = errors.New("component path rejected")
	// ErrComponentRejected reports a file that failed payload or structural
	// validation. Identity is deliberately not part of this: a supplement is not
	// the article.
	ErrComponentRejected = errors.New("component rejected")
)

// AdoptComponent files a supplement or appendix alongside a job's main artifact:
// the parts of a source that a quotation may legitimately live in, which a main
// PDF alone would report as absent (ADR-0007).
//
// Three deliberate differences from AdoptDownload, which handles the main file:
//
//   - The job must already hold its main artifact. A component is additional
//     evidence about an acquisition, never the acquisition itself, so there is no
//     state transition here and no path where a component can satisfy a job.
//   - Identity is NOT checked. Identity asks "are these bytes the work we asked
//     for", and a supplement is by definition not the article: it usually carries
//     neither the title nor the DOI. Requiring a match would reject every real
//     supplement, and asserting one would be a lie. Payload and structural
//     validation still apply in full — an unreadable or active-content PDF is
//     rejected exactly as for a main file.
//   - Only PDF roles are accepted. `html_fulltext` is deliberately refused: raw
//     provider HTML is inherently active content, and the artifact store's
//     integrity model assumes bounded, validated, inert files. Admitting it needs
//     a sanitization design, not a new role string.
//
// papio reports which components it holds; whether they make a source complete
// enough to support a negative finding is the consumer's judgement.
func (s *Service) AdoptComponent(ctx context.Context, jobID, path, role string) error {
	switch role {
	case job.ComponentSupplement, job.ComponentAppendix:
	case job.ComponentHTMLFullText:
		return fmt.Errorf("%w: %s requires sanitized storage papio does not have yet", ErrComponentRole, role)
	default:
		return fmt.Errorf("%w: %q", ErrComponentRole, role)
	}
	if s.Validate == nil {
		return errors.New("acquisition service is missing its validation dependency")
	}
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if row.ArtifactSHA256 == "" {
		return fmt.Errorf("%w: job %s holds no main artifact to attach a %s to", ErrComponentPrecondition, jobID, role)
	}

	// Same confinement as main adoption: ancestor symlinks resolve so mounts
	// work, but the final component is Lstat-checked so a symlinked file is
	// rejected rather than followed.
	// Only an input-policy failure may be reported as one. An EACCES or EIO on the
	// daemon's OWN adoption root is an operational fault, and telling the operator
	// "the file must be inside the adoption root" would send them to inspect a path
	// that is fine while hiding a broken data directory. Unexpected errors stay
	// unclassified so the RPC layer reports them as internal.
	realRoot, err := filepath.EvalSymlinks(filepath.Join(s.Config.EffectiveAdoptionRoot(), jobID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: job %s has no adoption root to attach a %s from", ErrComponentPrecondition, jobID, role)
		}
		return fmt.Errorf("adoption root unavailable: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		// A path the caller named that does not exist is the caller's mistake; any
		// other failure reading it is not something they can act on.
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", ErrComponentPath, err)
		}
		return fmt.Errorf("resolving component directory: %w", err)
	}
	resolved := filepath.Join(realDir, filepath.Base(path))
	// This one IS the policy check: escape, or a non-regular final component.
	if err := artifact.ConfineRegularFile(realRoot, resolved); err != nil {
		return fmt.Errorf("%w: %w", ErrComponentPath, err)
	}

	qdir, err := s.Artifacts.QuarantineDir(jobID)
	if err != nil {
		return err
	}
	temp := filepath.Join(qdir, job.NewID("component")+".tmp")
	sha, size, err := copyHashed(resolved, temp)
	if err != nil {
		return err
	}
	report, err := s.Validate(ctx, temp, sha, row.Work)
	if err != nil {
		_ = os.Remove(temp)
		return err
	}
	if !report.Payload.OK || !report.Structural.Valid {
		_ = os.Remove(temp)
		return fmt.Errorf("%w: %s is not a readable PDF", ErrComponentRejected, role)
	}
	// Same active-content predicate as main validation: JavaScript or embedded
	// files make a PDF a delivery vehicle rather than a document.
	if report.Structural.Encrypted || report.Structural.HasJavaScript || report.Structural.HasEmbeddedFiles {
		_ = os.Remove(temp)
		return fmt.Errorf("%w: %s is encrypted or carries active content", ErrComponentRejected, role)
	}

	dest, err := s.Artifacts.ArtifactPath(sha)
	if err != nil {
		_ = os.Remove(temp)
		return err
	}
	existing, err := s.Jobs.GetArtifact(ctx, sha)
	if err != nil {
		_ = os.Remove(temp)
		return err
	}
	// The component's own identity is not asserted, so its artifact row carries
	// no identity claim; the acquisition edge records the same absence.
	if err := s.Jobs.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: size, MIME: "application/pdf",
		PageCount: report.Structural.Pages, TextChars: report.Text.Chars, OCRUsed: report.Text.OCRUsed,
		Encrypted: false, HasActiveContent: false, Path: dest,
	}); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if _, err := s.Artifacts.Promote(temp, sha); err != nil {
		if existing == nil {
			if _, cleanupErr := s.Jobs.S.DB().ExecContext(context.WithoutCancel(ctx),
				`DELETE FROM artifacts WHERE sha256 = ?`, sha); cleanupErr != nil {
				return errors.Join(err, fmt.Errorf("removing unpromoted component metadata: %w", cleanupErr))
			}
		}
		return err
	}
	if err := s.Jobs.AddComponent(ctx, jobID, sha, role, 0, ""); err != nil {
		return err
	}
	return s.Jobs.RecordEvent(ctx, jobID, "acquisition.component_added", map[string]any{
		"role": role, "sha256": sha, "size_bytes": size, "page_count": report.Structural.Pages,
	})
}
