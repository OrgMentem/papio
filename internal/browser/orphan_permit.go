// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/job"
)

// An orphaned permit is an unknown_completion direct_get permit whose worker
// is gone. On 2026-09-23 an extension reload landed seconds after a SAGE
// direct route was offered: the browser saved an HTML page as the job's
// paper.pdf, no result reached the daemon, and the orphan held the global
// effect lane until an operator resolved it by hand. Every browser effect for
// every paper was refused as busy in the meantime. The sweep now settles such
// a permit from evidence it can read itself (internal/job's orphan rules say
// which), records that evidence, and leaves anything it cannot prove to
// doctor. Elapsed time alone never settles one.

type orphanPermitFileSeen struct {
	dir, name string
	rejected  bool
	size      int64
	modified  time.Time
	pdf       bool
}

type orphanPermitInspection struct {
	dirs  job.OrphanPermitDirectories
	files []orphanPermitFileSeen
}

// inspectOrphanPermitDirectories reads every directory the permit's download
// could have written: the job's adoption directory under each root, and the
// rejected/ sibling that holds bytes already moved aside. Only entries
// modified since the permit count; an older file cannot be its consequence.
// All filesystem work runs inside the adoption scan's deadline and gate, and
// an unreadable directory fails the whole inspection: it is not evidence that
// nothing is there.
func (b *Bridge) inspectOrphanPermitDirectories(permit *job.EffectPermit) (orphanPermitInspection, error) {
	since := permit.OrphanEvidenceSince()
	var out orphanPermitInspection
	visited := map[string]bool{}
	for _, root := range b.cfg.AdoptionRoots() {
		for _, rejected := range []bool{false, true} {
			dir := filepath.Join(root, permit.JobID)
			if rejected {
				dir = filepath.Join(root, "rejected", permit.JobID)
			}
			if visited[dir] {
				continue
			}
			visited[dir] = true
			var found []orphanPermitFileSeen
			var sincePermit, inProgress int
			_, err := b.readAdoptionDirWith(dir, func(dir string) ([]os.DirEntry, error) {
				readDir := b.readDir
				if readDir == nil {
					readDir = os.ReadDir
				}
				entries, err := readDir(dir)
				if err != nil {
					return nil, err
				}
				for _, e := range entries {
					name := e.Name()
					if strings.HasPrefix(name, ".") {
						continue
					}
					info, err := e.Info()
					if err != nil {
						return nil, err
					}
					if info.ModTime().Before(since) {
						continue
					}
					sincePermit++
					if strings.HasSuffix(name, ".crdownload") || strings.HasSuffix(name, ".download") ||
						strings.HasSuffix(name, ".part") || (info.Mode().IsRegular() && info.Size() == 0) {
						inProgress++ // the browser is still writing
						continue
					}
					if !info.Mode().IsRegular() {
						continue
					}
					found = append(found, orphanPermitFileSeen{
						dir: dir, name: name, rejected: rejected,
						size: info.Size(), modified: info.ModTime(),
						pdf: probeAdoptionPDF(dir, name),
					})
				}
				return entries, nil
			})
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return out, err
			}
			out.dirs.Inspected++
			out.dirs.EntriesSincePermit += sincePermit
			out.dirs.InProgress += inProgress
			out.files = append(out.files, found...)
		}
	}
	return out, nil
}

// recoverOrphanedEffectPermit settles the global lane's occupant when it is
// an orphaned direct_get permit and the evidence proves how its effect
// ended. It returns the permit when a PDF it produced waits in its job's
// adoption directory: the sweep then adopts that file with the permit's
// producer, and ordinary validation and producer correlation settle it.
func (b *Bridge) recoverOrphanedEffectPermit(ctx context.Context) *job.EffectPermit {
	if b.jobs == nil || b.adoptionLatchUnhealthy() {
		return nil
	}
	permit, err := b.jobs.LiveEffectPermit(ctx)
	if err != nil || !permit.OrphanRecoverable() {
		return nil
	}
	b.mu.Lock()
	generation := b.arbitration.generation()
	authorityKnown := !b.materializationGenerationUnavailable && !b.materializationAuthorityUncertain
	now := b.now()
	b.mu.Unlock()
	seen, err := b.inspectOrphanPermitDirectories(permit)
	if err != nil || seen.dirs.InProgress != 0 ||
		seen.dirs.EntriesSincePermit != len(seen.files) || len(seen.files) > 1 {
		return nil // unreadable, still downloading, or ambiguous: doctor reports it
	}
	if len(seen.files) == 0 {
		if !authorityKnown || generation <= permit.BrowserHolderGeneration ||
			permit.LeaseUntil == nil || now.Before(*permit.LeaseUntil) {
			return nil // the worker that owns it may still report
		}
		b.logOrphanPermitSettlement(permit, job.OrphanPermitNoEffectObserved,
			b.jobs.SettleOrphanedPermitNoEffect(ctx, permit.ID, generation, now, seen.dirs))
		return nil
	}
	f := seen.files[0]
	file := job.OrphanPermitFile{
		Filename: f.name, SizeBytes: f.size, ModifiedAt: f.modified,
		PDF: f.pdf, MovedAside: f.rejected,
	}
	rule := job.OrphanPermitEffectCompletedNoPaper
	if f.pdf {
		if f.rejected {
			return nil
		}
		row, err := b.jobs.Get(ctx, permit.JobID)
		if err != nil || row == nil {
			return nil
		}
		if sweepAdoptableState(row.State) {
			return permit
		}
		if !job.Terminal(row.State) {
			return nil
		}
		rule = job.OrphanPermitEffectCompletedJobTerminal
	}
	path := filepath.Join(f.dir, f.name)
	digest, err := fileDigest(path)
	if err != nil {
		return nil
	}
	file.SHA256 = digest
	if !f.pdf && !f.rejected {
		// The same move a rejected adoption makes: out of the adoption
		// directory, kept for the user, never re-adopted by the sweep.
		if err := b.preserveDeferredAdoption(permit.JobID, f.name, path); err != nil {
			log.Printf("papio: moving orphaned effect permit %s bytes aside: %v", permit.ID, err)
			return nil
		}
		file.MovedAside = true
	}
	b.logOrphanPermitSettlement(permit, rule,
		b.jobs.SettleOrphanedPermitFromFile(ctx, permit.ID, permit.JobID, rule, file))
	return nil
}

func (b *Bridge) logOrphanPermitSettlement(permit *job.EffectPermit, rule job.OrphanPermitRule, err error) {
	switch {
	case err == nil:
		log.Printf("papio: settled orphaned %s effect permit %s for job %s (%s); the effect lane is free",
			permit.Kind, permit.ID, permit.JobID, rule)
	case errors.Is(err, job.ErrEffectPermitStale), errors.Is(err, job.ErrOrphanPermitUnproven):
	default:
		log.Printf("papio: settling orphaned effect permit %s: %v", permit.ID, err)
	}
}

// attributeOrphanedArtifact returns the orphaned permit's producer for a PDF
// the sweep is about to adopt from the permit's job directory, after the
// store has recorded the attribution. Any doubt returns nil, and the file is
// adopted as an uncorrelated download exactly as before.
func (b *Bridge) attributeOrphanedArtifact(ctx context.Context, orphan *job.EffectPermit, name string) *job.ArtifactProducerIdentity {
	full, err := b.adoptionPath(orphan.JobID, name)
	if err != nil {
		return nil
	}
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	producer, err := b.jobs.AttributeOrphanedPermitArtifact(ctx, orphan.ID, orphan.JobID, job.OrphanPermitFile{
		Filename: name, SizeBytes: info.Size(), ModifiedAt: info.ModTime(), PDF: true,
	})
	if err != nil {
		if !errors.Is(err, job.ErrEffectPermitStale) && !errors.Is(err, job.ErrOrphanPermitUnproven) {
			log.Printf("papio: attributing %s to orphaned effect permit %s: %v", name, orphan.ID, err)
		}
		return nil
	}
	return producer
}
