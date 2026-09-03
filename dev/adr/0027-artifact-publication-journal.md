# ADR-0027: Artifact publication journal and two-resource finalization

Status: **Accepted** (2026-09-03). This decision defines the durable publication
protocol for main artifacts and adopted components.

## Context

Today, artifact metadata, filesystem bytes, candidate acceptance, the acquisition
edge, and the job transition can commit independently. A crash after a link or
rename can leave visible bytes without a durable owner. SQLite rollback cannot
undo filesystem publication.

The artifact store owns quarantine files, hashing, and promotion. The job store
owns artifact metadata, candidates, acquisition edges, leases, and job state.
A publication crosses both resources. It needs a durable intent before the
filesystem makes the bytes visible.

Main resolver acceptance and browser adoption already share one validation and
acceptance seam: `validateCandidate`. Component adoption uses the same artifact
rules, but it must not change the main job state.

## Decision 1: Prepare a durable publication before filesystem publication

A publication has two phases: preparation and finalization.

Preparation commits one journal row in the job store before promotion makes the
digest path visible. The row binds all of these facts:

- the job;
- the candidate when the publication has one, or the component role when it does
  not;
- the publication role, including `main` and the defined component roles;
- the content digest and quarantine path;
- complete artifact metadata;
- the optional adoption-lease owner; and
- the intended main transition and transition detail, when the role is `main`.

Preparation persists the artifact metadata and the binding with the journal row
in the same transaction. If a lease owner is present, preparation rechecks that
lease in that transaction. A failed lease check rejects preparation.

The committed journal is the durable owner of a prepared publication. Every
visible artifact file must therefore have either a prepared journal row or a
committed acquisition edge.

## Decision 2: Finalize under a writer fence

Finalization starts a SQLite write transaction. It acquires a SQLite write fence
before it calls the precomputed promotion callback. `BEGIN DEFERRED` alone does
not satisfy this rule.

The finalizer then rechecks the prepared row and its optional lease owner. It
calls exactly one precomputed promotion callback while the writer fence remains
held. The fence prevents another lease owner from replacing the lease during the
bounded link or rename operation.

After the callback succeeds, the finalizer completes these changes in the same
transaction:

1. It records candidate acceptance when the publication has a candidate.
2. It records the job-to-artifact acquisition edge.
3. It performs the legal job transition when the role is `main`.
4. It removes the prepared journal row.

A component publication records its acquisition edge and removes its journal
row. It does not transition the main job state. The finalizer must reject a
candidate that does not belong to the journal job.

A lease renewal failure stops the process before it enters finalization. The
finalizer does not use a lease check before promotion as a substitute for the
writer fence.

## Decision 3: Treat promotion and commit as separate effects

Promotion returns both its destination path and whether it created that digest
path. A reused digest path is shared content. Recovery and compensation must
never delete reused content.

A commit error after promotion is ambiguous. The finalizer must not assume that
SQLite rolled back. It opens a fresh job-store connection and reads the durable
acquisition edge and the journal row. It returns success only when the edge
committed.

If the edge did not commit, recovery handles the prepared row. It verifies the
destination when it exists. It retries promotion from a surviving quarantine
file through the idempotent promotion path. Recovery finalizes the same durable
intent; it does not create a replacement intent for the same publication.

If neither destination nor quarantine file survives, recovery may remove the
journal and artifact metadata only after one store transaction proves that no
acquisition edge or other publication intent references the digest. Filesystem
publication is not compensable by a destination delete.

## Decision 4: Preserve validation and component semantics

`validateCandidate` remains the single main-artifact validation and acceptance
seam. Main resolver acceptance calls it without an adoption lease. Browser
adoption calls it with its current adoption-lease owner. Both paths use the
publication protocol after validation.

Component adoption uses the same prepare and finalize protocol. It creates an
acquisition edge for its role. It does not replace the main artifact. It does
not transition the main job state.

The protocol preserves these invariants:

- Every visible artifact file has a prepared journal row or a committed
  acquisition edge.
- Every acquisition edge resolves to hash-verified content.
- Main resolver acceptance and browser adoption use `validateCandidate`.
- Component adoption has no main-job-state transition.
- Candidate acceptance always binds the candidate to its own job.

## Rejected alternatives

### Metadata before rename alone

Rejected. Metadata without a prepared publication intent does not name the
pending job, candidate or role, digest, quarantine file, lease fence, and
planned transition as one recoverable operation.

### Lease check before promotion without a transaction fence

Rejected. A lease can change after the check and during promotion. The writer
fence must cover the bounded promotion callback.

### Delete the destination as compensation

Rejected. A destination can hold content another job reused. Deletion can remove
shared content and cannot repair an uncertain database commit.

### Rely on SQLite rollback after filesystem publication

Rejected. SQLite cannot roll back a hard link or rename. Recovery must inspect
durable state and filesystem state separately.

## Consequences

The job store gains a narrow publication journal and finalization operation. The
artifact store continues to own hashing and filesystem promotion. The two stores
remain separate, but the journal gives each visible file a durable owner across
a process failure.

Fault tests must cover a stop after preparation, a stop after promotion before
commit, an ambiguous commit with fresh-connection recovery, digest reuse, lease
replacement before finalization, and stale recovery while promotion holds the
writer fence. They must also cover resolver publication without a lease, browser
adoption with a lease, and component adoption without a main-job transition.
