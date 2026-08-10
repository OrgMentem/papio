// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

/** The lifecycle of one institution-level federated-login claim. */
export type FederatedClaimPhase = "engaging" | "auth";

/** Persisted owner value. The claim key itself is opaque and never decoded here. */
export interface FederatedClaimOwner {
  jobID: string;
  tabID: number;
  phase: FederatedClaimPhase;
}

export type FederatedClaimOwners = Record<string, FederatedClaimOwner>;

/**
 * Reserve an unowned claim before the correlated daemon request starts.
 *
 * The returned map is the same object for a same-owner no-op, and undefined
 * means another job owns the claim. This makes the check-and-write operation
 * suitable for the Bridge's serialized state update path without allocating
 * for duplicate transitions.
 */
export function reserveFederatedClaim(
  owners: FederatedClaimOwners | undefined,
  claimKey: string,
  jobID: string,
): FederatedClaimOwners | undefined {
  const current = owners?.[claimKey];
  if (current !== undefined) {
    return current.jobID === jobID ? owners : undefined;
  }
  return { ...(owners ?? {}), [claimKey]: { jobID, tabID: -1, phase: "engaging" } };
}

/** Bind a reserved claim to its materialized browser tab synchronously. */
export function bindFederatedClaim(
  owners: FederatedClaimOwners | undefined,
  claimKey: string,
  jobID: string,
  tabID: number,
): FederatedClaimOwners | undefined {
  const current = owners?.[claimKey];
  if (current === undefined || current.jobID !== jobID) return undefined;
  if (current.tabID === tabID) return owners;
  if (current.phase !== "engaging" || current.tabID >= 0) return undefined;
  return { ...(owners ?? {}), [claimKey]: { ...current, tabID } };
}

/** Promote the same owner from cold engagement to in-flight provider auth. */
export function promoteFederatedClaim(
  owners: FederatedClaimOwners | undefined,
  claimKey: string,
  jobID: string,
): FederatedClaimOwners | undefined {
  const current = owners?.[claimKey];
  if (current === undefined || current.jobID !== jobID) return undefined;
  if (current.phase === "auth") return owners;
  return { ...(owners ?? {}), [claimKey]: { ...current, phase: "auth" } };
}

/** Release a claim only when the caller still owns it. */
export function releaseFederatedClaim(
  owners: FederatedClaimOwners | undefined,
  claimKey: string,
  jobID: string,
): FederatedClaimOwners | undefined {
  const current = owners?.[claimKey];
  if (current === undefined || current.jobID !== jobID) return owners;
  const next = { ...(owners ?? {}) };
  delete next[claimKey];
  return next;
}

/** Rollback is intentionally owner-only and equivalent to release. */
export const rollbackFederatedClaim = releaseFederatedClaim;
