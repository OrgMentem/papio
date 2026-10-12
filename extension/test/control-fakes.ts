// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Shared fakes for the signed-adapter-control tests (control.test.ts and the
// bridge-level cases in background.test.ts).

import {
  suspensionsDigest,
  type AdapterControlStorage,
  type PersistedAdapterControl,
} from "../src/control";
import type { AdapterControlResponsePayload } from "../src/protocol";

export class FakeControlStorage implements AdapterControlStorage {
  value: unknown;
  readonly saves: PersistedAdapterControl[] = [];
  failNextSave = false;
  failLoad = false;
  /** Runs at the instant a write is attempted, before it lands. */
  onSave: ((value: PersistedAdapterControl) => void) | undefined;
  constructor(value?: unknown) {
    this.value = value;
  }
  async load(): Promise<unknown> {
    if (this.failLoad) throw new Error("storage unavailable");
    return this.value;
  }
  async save(value: PersistedAdapterControl): Promise<void> {
    this.onSave?.(value);
    if (this.failNextSave) {
      this.failNextSave = false;
      throw new Error("quota exceeded");
    }
    this.value = JSON.parse(JSON.stringify(value));
    this.saves.push(value);
  }
}

/** A storage already holding `sequence` over `revisions`, as a worker that
 * applied it before dying would leave behind. */
export async function persistedControl(
  sequence: number,
  revisions: string[] = [],
): Promise<FakeControlStorage> {
  return new FakeControlStorage({
    last_sequence: sequence,
    last_verified_suspensions_digest: await suspensionsDigest(revisions, []),
  });
}

export async function currentResponse(
  sequence: number,
  revisions: string[] = [],
  domains: string[] = [],
  overrides: Partial<AdapterControlResponsePayload> = {},
): Promise<AdapterControlResponsePayload> {
  return {
    request_id: "request-control-0001",
    state: "current",
    control_sequence: sequence,
    suspensions_digest: await suspensionsDigest(revisions, domains),
    suspended_revisions: revisions,
    suspended_safety_domains: domains,
    ...overrides,
  };
}

export async function noneResponse(): Promise<AdapterControlResponsePayload> {
  return {
    request_id: "request-control-0001",
    state: "none",
    control_sequence: 0,
    suspensions_digest: await suspensionsDigest([], []),
    suspended_revisions: [],
    suspended_safety_domains: [],
  };
}

export async function unavailableResponse(): Promise<AdapterControlResponsePayload> {
  return { ...(await noneResponse()), state: "unavailable" };
}
