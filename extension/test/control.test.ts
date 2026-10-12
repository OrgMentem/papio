// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The signed-adapter-control state machine (src/control.ts), against a fake
// durable store. The daemon verifies the signature; these tests pin what the
// extension does with the verified answer: fail closed, never roll back, and
// write the fence BEFORE applying anything it permits.

import { createHash } from "node:crypto";
import { expect, test } from "bun:test";

import {
  AdapterControl,
  ADAPTER_CONTROL_STORAGE_KEY,
  decideAdapterControl,
  suspensionsDigest,
  type PersistedAdapterControl,
} from "../src/control";
import {
  FakeControlStorage,
  currentResponse as current,
  noneResponse as none,
  persistedControl,
  unavailableResponse as unavailable,
} from "./control-fakes";

/** Digest vectors computed by Go's protocol.SuspensionsDigest (the
 * authoritative implementation). Both sides pin the same hex. */
const EMPTY_SET_DIGEST =
  "efcac454b135229c85b492666dc43af4fdfe5e58afe2e7937b31e404ce1d2f27";
const SCIENCEDIRECT_DIGEST =
  "d9ada45fb9029f57dacec82802fed8b4472457d678544901e1f283057552b028";

const SCIENCEDIRECT = { id: "sciencedirect", version: "0.3.0", hosts: ["sciencedirect.com"] };
const SCIENCEDIRECT_NEWER = { id: "sciencedirect", version: "0.4.0", hosts: ["sciencedirect.com"] };
const JSTOR = { id: "jstor", version: "0.3.2", hosts: ["jstor.org", "www.jstor.org"] };
const REGISTRY = [SCIENCEDIRECT, SCIENCEDIRECT_NEWER, JSTOR];

/** A control that has already persisted `sequence` over `revisions`, on a
 * fresh connection that has not heard back yet (a worker restart). */
async function restartedAt(
  sequence: number,
  revisions: string[] = [],
): Promise<{ control: AdapterControl; storage: FakeControlStorage; epoch: number }> {
  const storage = await persistedControl(sequence, revisions);
  const control = new AdapterControl(storage);
  await control.load();
  return { control, storage, epoch: control.beginConnection() };
}

test("digest vectors are identical to Go's SuspensionsDigest, independent of input order", async () => {
  expect(await suspensionsDigest([], [])).toBe(EMPTY_SET_DIGEST);
  expect(
    await suspensionsDigest(["sciencedirect@0.3.0"], ["publisher:www.sciencedirect.com"]),
  ).toBe(SCIENCEDIRECT_DIGEST);

  // The vector is a literal sha256 of the documented byte string, not an
  // artifact of this module's own helper.
  const literal = (text: string) => createHash("sha256").update(text).digest("hex");
  expect(literal("papio-suspensions/1\n")).toBe(EMPTY_SET_DIGEST);
  expect(
    literal(
      "papio-suspensions/1\nr sciencedirect@0.3.0\nd publisher:www.sciencedirect.com\n",
    ),
  ).toBe(SCIENCEDIRECT_DIGEST);

  // Byte-wise sort inside each list, revisions before domains.
  const sorted = await suspensionsDigest(["a@1.0.0", "b@1.0.0"], ["k:a", "k:b"]);
  expect(await suspensionsDigest(["b@1.0.0", "a@1.0.0"], ["k:b", "k:a"])).toBe(sorted);
  expect(literal("papio-suspensions/1\nr a@1.0.0\nr b@1.0.0\nd k:a\nd k:b\n")).toBe(sorted);

  // A revision and a domain are not interchangeable: the line tag is hashed.
  expect(await suspensionsDigest(["a@1.0.0"], [])).not.toBe(
    await suspensionsDigest([], ["a@1.0.0"]),
  );
});

test("a hello_ack verdict for a replaced connection does not change the live phase", async () => {
  const { control, epoch: stale } = await restartedAt(3);
  const live = control.beginConnection();

  control.markUnsupported(stale);
  expect(control.currentPhase()).toBe("unresolved");
  await control.resolve(await current(3), live);
  expect(control.currentPhase()).toBe("applied");

  // Likewise once applied: a late verdict from the old port cannot unapply it.
  control.markUnsupported(stale);
  expect(control.currentPhase()).toBe("applied");
  control.markUnsupported(live);
  expect(control.currentPhase()).toBe("unsupported");
});

test("a fresh install with daemon state none applies and stays open without writing anything", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();

  // Before any answer, and with no daemon support at all, a fresh install
  // behaves exactly as it did before signed control existed.
  expect(control.currentPhase()).toBe("unresolved");
  expect(control.blocksPositiveEffects()).toBe(false);
  control.markUnsupported(epoch);
  expect(control.currentPhase()).toBe("unsupported");
  expect(control.blocksPositiveEffects()).toBe(false);

  const second = control.beginConnection();
  const resolution = await control.resolve(await none(), second);
  expect(resolution).toEqual({ outcome: "applied", control_sequence: 0, current: true });
  expect(control.currentPhase()).toBe("applied");
  expect(control.blocksPositiveEffects()).toBe(false);
  expect(control.suspendedAdapters(REGISTRY)).toEqual([]);
  expect(storage.saves).toEqual([]);
  expect(storage.value).toBeUndefined();
});

test("a higher sequence is persisted before it is applied, and only the two fence fields are stored", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();
  const observedAtWrite: Array<{ phase: string; suspended: boolean; blocks: boolean }> = [];
  storage.onSave = () => {
    observedAtWrite.push({
      phase: control.currentPhase(),
      suspended: control.isSuspended(SCIENCEDIRECT),
      blocks: control.blocksPositiveEffects(),
    });
  };

  const resolution = await control.resolve(
    await current(3, ["sciencedirect@0.3.0"], ["publisher:www.sciencedirect.com"]),
    epoch,
  );

  expect(resolution).toEqual({ outcome: "applied", control_sequence: 3, current: true });
  // At the instant of the write nothing the sequence permits was in effect.
  expect(observedAtWrite).toEqual([{ phase: "unresolved", suspended: false, blocks: false }]);
  expect(control.currentPhase()).toBe("applied");
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(true);
  expect(storage.value).toEqual({
    last_sequence: 3,
    last_verified_suspensions_digest: SCIENCEDIRECT_DIGEST,
  });
  expect(ADAPTER_CONTROL_STORAGE_KEY).toBe("papio_adapter_control_v1");
});

test("a failed fence write applies nothing and reports unavailable", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();
  storage.failNextSave = true;

  const resolution = await control.resolve(await current(2, ["sciencedirect@0.3.0"]), epoch);

  expect(resolution).toEqual({ outcome: "unavailable", control_sequence: 0, current: true });
  expect(control.currentPhase()).toBe("failed");
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(false);
  expect(control.blocksPositiveEffects()).toBe(true);
  expect(storage.value).toBeUndefined();
});

test("the same sequence with the same digest applies again without a write", async () => {
  const { control, storage, epoch } = await restartedAt(4, ["sciencedirect@0.3.0"]);

  const resolution = await control.resolve(await current(4, ["sciencedirect@0.3.0"]), epoch);

  expect(resolution).toEqual({ outcome: "applied", control_sequence: 4, current: true });
  expect(control.currentPhase()).toBe("applied");
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(true);
  expect(storage.saves).toEqual([]);
});

test("the same sequence naming a different suspension set is a digest mismatch", async () => {
  const { control, storage, epoch } = await restartedAt(4, ["sciencedirect@0.3.0"]);

  // Self-consistent (its lists hash to its digest) but not what sequence 4
  // verified: the daemon is presenting two sets under one sequence.
  const resolution = await control.resolve(await current(4, ["jstor@0.3.2"]), epoch);

  expect(resolution).toEqual({ outcome: "digest_mismatch", control_sequence: 4, current: true });
  expect(control.currentPhase()).toBe("failed");
  expect(control.blocksPositiveEffects()).toBe(true);
  expect(control.isSuspended(JSTOR)).toBe(false);
  expect(storage.saves).toEqual([]);
});

test("a lower sequence than one already seen is a rollback", async () => {
  const { control, storage, epoch } = await restartedAt(5, ["sciencedirect@0.3.0"]);

  const resolution = await control.resolve(await current(4, []), epoch);

  expect(resolution).toEqual({ outcome: "rolled_back", control_sequence: 5, current: true });
  expect(control.currentPhase()).toBe("failed");
  expect(control.blocksPositiveEffects()).toBe(true);
  expect(storage.saves).toEqual([]);
});

test("state none after a sequence was persisted is a missing control, not a clean slate", async () => {
  const { control, storage, epoch } = await restartedAt(2);

  const resolution = await control.resolve(await none(), epoch);

  expect(resolution).toEqual({ outcome: "missing", control_sequence: 2, current: true });
  expect(control.currentPhase()).toBe("failed");
  expect(control.blocksPositiveEffects()).toBe(true);
  expect(storage.saves).toEqual([]);
});

test("state unavailable fails closed even on a fresh install", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();
  expect(control.blocksPositiveEffects()).toBe(false);

  const resolution = await control.resolve(await unavailable(), epoch);

  expect(resolution).toEqual({ outcome: "unavailable", control_sequence: 0, current: true });
  expect(control.currentPhase()).toBe("failed");
  expect(control.blocksPositiveEffects()).toBe(true);
  expect(storage.saves).toEqual([]);

  // The next connection starts open again: a fresh install has no fence.
  control.beginConnection();
  expect(control.blocksPositiveEffects()).toBe(false);
});

test("a digest that does not match the received lists is rejected before anything is persisted", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();

  // A higher sequence that WOULD persist, carrying the digest of a different
  // set: the lists the daemon sent are not the lists it vouched for.
  const tampered = await current(9, ["sciencedirect@0.3.0"], [], {
    suspensions_digest: await suspensionsDigest(["jstor@0.3.2"], []),
  });
  const resolution = await control.resolve(tampered, epoch);

  expect(resolution).toEqual({ outcome: "digest_mismatch", control_sequence: 0, current: true });
  expect(control.currentPhase()).toBe("failed");
  expect(control.blocksPositiveEffects()).toBe(true);
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(false);
  expect(storage.saves).toEqual([]);
  expect(storage.value).toBeUndefined();
});

test("a higher sequence with empty lists lifts the suspension", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  let epoch = control.beginConnection();
  await control.resolve(await current(1, ["sciencedirect@0.3.0"]), epoch);
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(true);
  expect(control.suspendedHosts(REGISTRY)).toEqual(["sciencedirect.com"]);

  // Same connection: the lifting sequence replaces the set, not unions it.
  const lifted = await control.resolve(await current(2), epoch);

  expect(lifted).toEqual({ outcome: "applied", control_sequence: 2, current: true });
  expect(control.currentPhase()).toBe("applied");
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(false);
  expect(control.suspendedAdapters(REGISTRY)).toEqual([]);
  expect(control.suspendedHosts(REGISTRY)).toEqual([]);
  expect(control.blocksPositiveEffects()).toBe(false);
  expect(storage.value).toEqual({
    last_sequence: 2,
    last_verified_suspensions_digest: EMPTY_SET_DIGEST,
  });

  // Surviving a restart: the fence stays, the suspension set does not.
  epoch = control.beginConnection();
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(false);
  expect(control.blocksPositiveEffects()).toBe(true);
});

test("only the exact named revision is suspended", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();

  // A safety-domain entry is the daemon's to enforce; it must not suspend
  // anything in the extension's registry by name.
  await control.resolve(
    await current(1, ["sciencedirect@0.3.0"], ["publisher:www.jstor.org"]),
    epoch,
  );

  expect(control.isSuspended(SCIENCEDIRECT)).toBe(true);
  // Same adapter id, a different packaged version: not the named revision.
  expect(control.isSuspended(SCIENCEDIRECT_NEWER)).toBe(false);
  expect(control.isSuspended(JSTOR)).toBe(false);
  expect(control.suspendedAdapters(REGISTRY)).toEqual([SCIENCEDIRECT]);
  expect(control.suspendedHosts(REGISTRY)).toEqual(["sciencedirect.com"]);
});

test("suspended hosts are lowercase, sorted and unique across suspended adapters", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();
  await control.resolve(await current(1, ["jstor@0.3.2", "zeta@1.0.0"]), epoch);

  const registry = [
    JSTOR,
    { id: "zeta", version: "1.0.0", hosts: ["Z.example.org", "jstor.org"] },
    { id: "idle", version: "1.0.0", hosts: ["a.example.org"] },
  ];

  expect(control.suspendedHosts(registry)).toEqual(["jstor.org", "www.jstor.org", "z.example.org"]);
});

test("after a restart a persisted sequence blocks effects until a response applies", async () => {
  const { control, epoch } = await restartedAt(7);

  // Unresolved, and then told the daemon has no control support: both block,
  // because a sequence has been seen.
  expect(control.currentPhase()).toBe("unresolved");
  expect(control.blocksPositiveEffects()).toBe(true);
  control.markUnsupported(epoch);
  expect(control.currentPhase()).toBe("unsupported");
  expect(control.blocksPositiveEffects()).toBe(true);

  const next = control.beginConnection();
  expect(control.blocksPositiveEffects()).toBe(true);
  await control.resolve(await current(7), next);
  expect(control.currentPhase()).toBe("applied");
  expect(control.blocksPositiveEffects()).toBe(false);

  // A new connection forgets the verdict, not the fence.
  control.beginConnection();
  expect(control.currentPhase()).toBe("unresolved");
  expect(control.blocksPositiveEffects()).toBe(true);
});

test("an unreadable fence fails closed until a verified current response re-establishes it", async () => {
  for (const unreadable of [
    { last_sequence: -1, last_verified_suspensions_digest: "" },
    { last_sequence: 2, last_verified_suspensions_digest: "not-a-digest" },
    { last_sequence: 1.5, last_verified_suspensions_digest: EMPTY_SET_DIGEST },
    { last_sequence: 0, last_verified_suspensions_digest: EMPTY_SET_DIGEST },
    "garbage",
    null,
  ]) {
    const storage = new FakeControlStorage(unreadable);
    const control = new AdapterControl(storage);
    await control.load();
    const epoch = control.beginConnection();
    expect(control.blocksPositiveEffects(), JSON.stringify(unreadable)).toBe(true);
    expect(control.requestFields()).toEqual({ last_control_sequence: 0 });

    // None cannot prove nothing was ever published.
    expect((await control.resolve(await none(), epoch)).outcome).toBe("missing");
    expect(control.blocksPositiveEffects()).toBe(true);

    const recovered = await control.resolve(await current(3), control.beginConnection());
    expect(recovered.outcome).toBe("applied");
    expect(storage.value).toEqual({
      last_sequence: 3,
      last_verified_suspensions_digest: EMPTY_SET_DIGEST,
    });
    expect(control.blocksPositiveEffects()).toBe(false);
  }

  const throwing = new FakeControlStorage();
  throwing.failLoad = true;
  const control = new AdapterControl(throwing);
  await control.load();
  control.beginConnection();
  expect(control.blocksPositiveEffects()).toBe(true);
  // A transient read failure heals on the next resolution.
  throwing.failLoad = false;
  const epoch = control.beginConnection();
  expect((await control.resolve(await none(), epoch)).outcome).toBe("applied");
  expect(control.blocksPositiveEffects()).toBe(false);
});

test("a response for a replaced connection advances the fence but never the live phase", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const stale = control.beginConnection();
  const live = control.beginConnection();

  const resolution = await control.resolve(await current(6, ["sciencedirect@0.3.0"]), stale);

  expect(resolution).toEqual({ outcome: "applied", control_sequence: 6, current: false });
  expect(storage.value).toEqual({
    last_sequence: 6,
    last_verified_suspensions_digest: await suspensionsDigest(["sciencedirect@0.3.0"], []),
  });
  expect(control.currentPhase()).toBe("unresolved");
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(false);
  // The fence moved, so the live connection now needs its own answer.
  expect(control.blocksPositiveEffects()).toBe(true);
  expect((await control.resolve(await current(6, ["sciencedirect@0.3.0"]), live)).current).toBe(true);
});

test("overlapping responses are applied one at a time in the order they arrive", async () => {
  const storage = new FakeControlStorage();
  const control = new AdapterControl(storage);
  await control.load();
  const epoch = control.beginConnection();

  const first = control.resolve(await current(2, ["sciencedirect@0.3.0"]), epoch);
  const older = control.resolve(await current(1), epoch);
  const results = await Promise.all([first, older]);

  expect(results.map((result) => result.outcome)).toEqual(["applied", "rolled_back"]);
  expect(control.currentPhase()).toBe("failed");
});

test("requests echo the persisted fence and omit the digest at sequence 0", async () => {
  const fresh = new AdapterControl(new FakeControlStorage());
  await fresh.load();
  expect(fresh.requestFields()).toEqual({ last_control_sequence: 0 });

  const { control } = await restartedAt(9, ["sciencedirect@0.3.0"]);
  expect(control.requestFields()).toEqual({
    last_control_sequence: 9,
    last_suspensions_digest: await suspensionsDigest(["sciencedirect@0.3.0"], []),
  });
});

test("the rule table is ordered: unavailable, digest, none, rollback, equal-digest, advance", () => {
  const persisted: PersistedAdapterControl = {
    last_sequence: 5,
    last_verified_suspensions_digest: "a".repeat(64),
  };
  const response = (state: "none" | "current" | "unavailable", sequence: number, digest = "a".repeat(64)) => ({
    state,
    control_sequence: sequence,
    suspensions_digest: digest,
  });

  // unavailable outranks a bad digest and a rollback.
  expect(decideAdapterControl(persisted, response("unavailable", 0), false)).toEqual({
    kind: "failed",
    outcome: "unavailable",
  });
  // A bad digest outranks a rollback, a mismatch and an advance.
  for (const sequence of [0, 3, 5, 9]) {
    expect(decideAdapterControl(persisted, response("current", sequence), false)).toEqual({
      kind: "failed",
      outcome: "digest_mismatch",
    });
  }
  expect(decideAdapterControl(persisted, response("none", 0), true)).toEqual({
    kind: "failed",
    outcome: "missing",
  });
  expect(decideAdapterControl(persisted, response("current", 4), true)).toEqual({
    kind: "failed",
    outcome: "rolled_back",
  });
  expect(decideAdapterControl(persisted, response("current", 5, "b".repeat(64)), true)).toEqual({
    kind: "failed",
    outcome: "digest_mismatch",
  });
  expect(decideAdapterControl(persisted, response("current", 5), true)).toEqual({
    kind: "applied",
    sequence: 5,
    digest: "a".repeat(64),
    persist: false,
  });
  expect(decideAdapterControl(persisted, response("current", 6, "c".repeat(64)), true)).toEqual({
    kind: "applied",
    sequence: 6,
    digest: "c".repeat(64),
    persist: true,
  });
});

test("invalidate returns an applied connection to unresolved, keeps a failed one failed, and ignores a stale epoch", async () => {
  const { control, epoch } = await restartedAt(3);
  await control.resolve(await current(3), epoch);
  expect(control.blocksPositiveEffects()).toBe(false);

  control.invalidate(epoch - 1);
  expect(control.currentPhase()).toBe("applied");

  control.invalidate(epoch);
  expect(control.currentPhase()).toBe("unresolved");
  expect(control.blocksPositiveEffects()).toBe(true);
  expect(control.isSuspended(SCIENCEDIRECT)).toBe(false);

  const failedEpoch = control.beginConnection();
  await control.resolve(await current(2), failedEpoch);
  expect(control.currentPhase()).toBe("failed");
  control.invalidate(failedEpoch);
  expect(control.currentPhase()).toBe("failed");
});
