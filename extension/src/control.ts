// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Signed adapter control, extension side (ADR-0021 Decision 4).
//
// The daemon verifies an Ed25519-signed document and holds the canonical
// control. The extension NEVER verifies a signature: it trusts the daemon over
// the native host, persists only `{last_sequence,
// last_verified_suspensions_digest}`, and fails CLOSED. A published sequence
// can only ever restrict (suspend a packaged adapter revision) and a sequence
// the extension has already seen can never be replayed back to an older one.
//
// This module is the whole state machine and nothing else: it holds no port,
// no timer, and no tab. `background.ts` owns the wiring and asks it two
// questions — "may a positive effect run at all" and "is this packaged
// adapter revision suspended".

import type { AdapterSpec } from "./adapters/types";
import type {
  AdapterControlOutcome,
  AdapterControlRequestPayload,
  AdapterControlResponsePayload,
} from "./protocol";

/** chrome.storage.local key. Dedicated: NOT the managed-state object (which an
 * update or a "clear state" path rewrites wholesale) and NOT the session
 * backend (wiped by a browser restart, which would erase the rollback fence). */
export const ADAPTER_CONTROL_STORAGE_KEY = "papio_adapter_control_v1";

/** Name a correlated request policy uses to say "send only after a hello_ack
 * with `adapter_control: true` on the current port". It is not a daemon
 * feature string: the daemon's feature list is full, so this capability rides
 * a flag on hello_ack instead, and `background.ts` resolves this name from
 * that flag alone — never from `features`, which could otherwise spoof it. */
export const ADAPTER_CONTROL_GATE = "hello_ack:adapter_control";

/** The only thing persisted: a monotone fence and the digest of the suspension
 * set that fence last verified. Suspension SETS are deliberately not stored;
 * they are re-derived from the daemon on every connection. */
export interface PersistedAdapterControl {
  last_sequence: number;
  last_verified_suspensions_digest: string;
}

export interface AdapterControlStorage {
  /** Raw stored value; `undefined` when the key is absent. May throw. */
  load(): Promise<unknown>;
  save(value: PersistedAdapterControl): Promise<void>;
}

/** Runtime phase of ONE native connection. Reset to "unresolved" on every
 * (re)connect. "unsupported" is a daemon whose hello_ack did not advertise
 * `adapter_control` (daemon_without_control). */
export type AdapterControlPhase =
  | "unresolved"
  | "applied"
  | "failed"
  | "unsupported";

export type AdapterControlFailure = Exclude<AdapterControlOutcome, "applied">;

export interface AdapterControlResolution {
  outcome: AdapterControlOutcome;
  /** The sequence this extension applied, or its persisted one on failure. */
  control_sequence: number;
  /** False when the connection that asked has since been replaced: the
   * persisted fence may still have advanced (it is monotone and daemon
   * verified), but this resolution must not change the live phase and no
   * apply_result may be sent for it. */
  current: boolean;
}

/** What the popup says about control, in plain words. Absent means nothing
 * unusual. `paused` is the whole-extension fail-closed state; `suspended`
 * lists the packaged adapter ids held back for the human. */
export type AdapterControlNotice =
  | { kind: "paused" }
  | { kind: "suspended"; adapters: string[] };

export type AdapterControlDecision =
  | {
      kind: "applied";
      sequence: number;
      digest: string;
      /** True when the fence must be advanced BEFORE anything is applied. */
      persist: boolean;
    }
  | { kind: "failed"; outcome: AdapterControlFailure };

const DIGEST_RE = /^[0-9a-f]{64}$/;
const DIGEST_DOMAIN = "papio-suspensions/1\n";

const ZERO: PersistedAdapterControl = Object.freeze({
  last_sequence: 0,
  last_verified_suspensions_digest: "",
});

/** The canonical suspension-set digest, identical to Go's
 * `protocol.SuspensionsDigest`:
 *   sha256("papio-suspensions/1\n" + "r <revision>\n"… + "d <domain>\n"…)
 * with both lists sorted byte-wise (every accepted ID is ASCII, so the default
 * code-unit sort is the same order) and the result lowercase hex. */
export async function suspensionsDigest(
  revisions: readonly string[],
  domains: readonly string[],
): Promise<string> {
  const text =
    DIGEST_DOMAIN +
    [...revisions]
      .sort()
      .map((revision) => `r ${revision}\n`)
      .join("") +
    [...domains]
      .sort()
      .map((domain) => `d ${domain}\n`)
      .join("");
  const digest = await globalThis.crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(text),
  );
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

/** A suspended revision is exactly one packaged adapter at one version. */
export function adapterRevisionID(spec: Pick<AdapterSpec, "id" | "version">): string {
  return `${spec.id}@${spec.version}`;
}

/** The pure rule table, in the contract's order. `persisted` is `undefined`
 * when the stored record could not be read: nothing can then be proven about
 * the past, so only a verified `current` response may re-establish a fence.
 * `digestMatches` is whether the digest recomputed from the received lists
 * equals `response.suspensions_digest`. */
export function decideAdapterControl(
  persisted: PersistedAdapterControl | undefined,
  response: Pick<
    AdapterControlResponsePayload,
    "state" | "control_sequence" | "suspensions_digest"
  >,
  digestMatches: boolean,
): AdapterControlDecision {
  // 1. The daemon cannot supply canonical state.
  if (response.state === "unavailable")
    return { kind: "failed", outcome: "unavailable" };
  // 2. The lists the daemon sent do not hash to the digest it claims.
  if (!digestMatches) return { kind: "failed", outcome: "digest_mismatch" };
  const sequence = response.control_sequence;
  // 3. Nothing published. Fine on a fresh install; a REGRESSION otherwise,
  // because a daemon that once published control and now says "none" has lost
  // or hidden it.
  if (sequence === 0) {
    if (persisted === undefined || persisted.last_sequence > 0)
      return { kind: "failed", outcome: "missing" };
    return {
      kind: "applied",
      sequence: 0,
      digest: response.suspensions_digest,
      persist: false,
    };
  }
  // An unreadable record proves nothing about the past: a verified current
  // response re-establishes the fence.
  if (persisted === undefined || sequence > persisted.last_sequence) {
    return {
      kind: "applied",
      sequence,
      digest: response.suspensions_digest,
      persist: true,
    };
  }
  // 4. An older sequence than one already seen is a rollback.
  if (sequence < persisted.last_sequence)
    return { kind: "failed", outcome: "rolled_back" };
  // 5. The same sequence must name the same suspension set.
  if (response.suspensions_digest !== persisted.last_verified_suspensions_digest)
    return { kind: "failed", outcome: "digest_mismatch" };
  return {
    kind: "applied",
    sequence,
    digest: response.suspensions_digest,
    persist: false,
  };
}

/** `undefined` for a record that is present but malformed. */
function parsePersisted(raw: unknown): PersistedAdapterControl | undefined {
  if (raw === undefined) return ZERO;
  if (typeof raw !== "object" || raw === null || Array.isArray(raw))
    return undefined;
  const record = raw as Record<string, unknown>;
  const sequence = record["last_sequence"];
  const digest = record["last_verified_suspensions_digest"];
  if (
    typeof sequence !== "number" ||
    !Number.isSafeInteger(sequence) ||
    sequence < 0 ||
    typeof digest !== "string"
  )
    return undefined;
  if (sequence > 0 ? !DIGEST_RE.test(digest) : digest !== "") return undefined;
  return { last_sequence: sequence, last_verified_suspensions_digest: digest };
}

export class AdapterControl {
  /** `undefined` until the first successful read, and again once a read
   * failed or found a malformed record. Absence of the key is NOT that: a
   * fresh install reads as sequence 0. */
  private persisted: PersistedAdapterControl | undefined;
  private phase: AdapterControlPhase = "unresolved";
  private revisions: ReadonlySet<string> = new Set();
  private epoch = 0;
  private chain: Promise<unknown> = Promise.resolve();

  /** With no storage the fence lives in memory only: the state a build with
   * no durable seam (a unit-test harness) is in, which behaves exactly like a
   * fresh install until a higher sequence arrives. */
  constructor(private readonly storage?: AdapterControlStorage) {
    this.persisted = storage === undefined ? ZERO : undefined;
  }

  /** Read the persisted fence. Never throws: an unreadable record leaves the
   * fence unknown, which blocks positive effects until a verified `current`
   * response re-establishes it. */
  async load(): Promise<void> {
    if (this.persisted !== undefined || this.storage === undefined) return;
    try {
      this.persisted = parsePersisted(await this.storage.load());
    } catch {
      this.persisted = undefined;
    }
  }

  /** A new native connection: nothing about the last one carries over except
   * the persisted fence. Returns the epoch the connection's requests must
   * present to `resolve`. */
  beginConnection(): number {
    this.epoch += 1;
    this.phase = "unresolved";
    this.revisions = new Set();
    return this.epoch;
  }

  /** hello_ack on the current connection did not advertise adapter_control. */
  markUnsupported(epoch: number): void {
    if (epoch !== this.epoch) return;
    this.phase = "unsupported";
    this.revisions = new Set();
  }

  /** The current connection can no longer vouch for what it applied (a
   * refresh failed, or the port closed): back to unresolved, which blocks
   * positive effects whenever a sequence has been seen. A failed phase stays
   * failed. Stale epochs are ignored. */
  invalidate(epoch: number): void {
    if (epoch !== this.epoch || this.phase === "failed") return;
    this.phase = "unresolved";
    this.revisions = new Set();
  }

  currentPhase(): AdapterControlPhase {
    return this.phase;
  }

  /** What a request must echo: the persisted fence, or sequence 0 when
   * nothing usable is persisted (the digest is omitted at sequence 0). */
  requestFields(): Pick<
    AdapterControlRequestPayload,
    "last_control_sequence" | "last_suspensions_digest"
  > {
    const persisted = this.persisted;
    if (persisted === undefined || persisted.last_sequence === 0)
      return { last_control_sequence: 0 };
    return {
      last_control_sequence: persisted.last_sequence,
      last_suspensions_digest: persisted.last_verified_suspensions_digest,
    };
  }

  /** True when no positive effect (tab open, click, download, terms accept,
   * login) may run. A fresh install (sequence 0) stays open whether the
   * daemon says "none" or does not speak control at all; once a sequence has
   * been seen, only a current "applied" resolution reopens it. */
  blocksPositiveEffects(): boolean {
    if (this.phase === "failed") return true;
    if (this.phase === "applied") return false;
    return this.persisted === undefined || this.persisted.last_sequence > 0;
  }

  /** A packaged adapter revision the applied control suspends. */
  isSuspended(spec: Pick<AdapterSpec, "id" | "version">): boolean {
    return this.phase === "applied" && this.revisions.has(adapterRevisionID(spec));
  }

  /** The packaged adapters the applied control suspends, in registry order. */
  suspendedAdapters<T extends Pick<AdapterSpec, "id" | "version">>(
    specs: readonly T[],
  ): T[] {
    if (this.phase !== "applied" || this.revisions.size === 0) return [];
    return specs.filter((spec) => this.isSuspended(spec));
  }

  /** Lowercase, sorted, unique hostnames of the suspended packaged adapters:
   * the apply_result's `suspended_hosts`. Derived from the extension's OWN
   * registry, never from the signed document. */
  suspendedHosts(specs: readonly Pick<AdapterSpec, "id" | "version" | "hosts">[]): string[] {
    const hosts = new Set<string>();
    for (const spec of this.suspendedAdapters(specs))
      for (const host of spec.hosts) hosts.add(host.toLowerCase());
    return [...hosts].sort();
  }

  /** Apply one daemon response. Responses are serialized so two overlapping
   * requests cannot interleave their load/compare/persist steps. Never
   * throws. */
  resolve(
    response: AdapterControlResponsePayload,
    epoch: number,
  ): Promise<AdapterControlResolution> {
    const run = this.chain.then(() => this.resolveNow(response, epoch));
    this.chain = run.catch(() => {});
    return run;
  }

  private async resolveNow(
    response: AdapterControlResponsePayload,
    epoch: number,
  ): Promise<AdapterControlResolution> {
    await this.load();
    let digestMatches = false;
    try {
      digestMatches =
        (await suspensionsDigest(
          response.suspended_revisions,
          response.suspended_safety_domains,
        )) === response.suspensions_digest;
    } catch {
      // No way to verify is no verification.
      digestMatches = false;
    }
    const decision = decideAdapterControl(this.persisted, response, digestMatches);
    if (decision.kind === "failed")
      return this.settleFailed(decision.outcome, epoch);
    if (decision.persist) {
      // Advance the fence BEFORE applying anything it permits: a crash after
      // this write can only leave the extension stricter, never looser.
      try {
        await this.persist({
          last_sequence: decision.sequence,
          last_verified_suspensions_digest: decision.digest,
        });
      } catch {
        // The fence did not advance, so nothing under it may be applied. The
        // closed outcome vocabulary has no storage failure; "unavailable"
        // says what is true — canonical state could not be applied.
        return this.settleFailed("unavailable", epoch);
      }
    }
    const current = epoch === this.epoch;
    if (current) {
      this.phase = "applied";
      this.revisions = new Set(response.suspended_revisions);
    }
    return { outcome: "applied", control_sequence: decision.sequence, current };
  }

  private async persist(next: PersistedAdapterControl): Promise<void> {
    if (this.storage !== undefined) await this.storage.save(next);
    this.persisted = next;
  }

  private settleFailed(
    outcome: AdapterControlFailure,
    epoch: number,
  ): AdapterControlResolution {
    const current = epoch === this.epoch;
    if (current) {
      this.phase = "failed";
      this.revisions = new Set();
    }
    return {
      outcome,
      control_sequence: this.persisted?.last_sequence ?? 0,
      current,
    };
  }
}
