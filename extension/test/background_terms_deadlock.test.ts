// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Regression coverage for correlated terms replies on the serialized native FIFO.

import { expect, test } from "bun:test";
import { Window } from "happy-dom";

import {
  assessDrivenPage,
  Bridge,
  executePlannedPageEffect,
  MIN_DAEMON_VERSION,
  type BridgeDeps,
  type NativePort,
} from "../src/background";
import type { AdapterSpec } from "../src/adapters/types";
import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { planExecution, type Plan } from "../src/plan";
import {
  emptyStore,
  type StateBackend,
  type StoreShape,
} from "../src/state";
import { FakeDownloads } from "./fake-downloads";
import { ChromeTabsFake, FakeEmitter } from "./fake-tabs";

const JOB_ID = "job_restored_terms_deadlock";
const TAB_ID = 77;
const PROVIDER = "www.jstor.org";
const PROVIDER_URL = `https://${PROVIDER}/stable/4093878`;
const OPENURL = "https://resolver.example.edu/openurl?ctx=terms-deadlock";

const TERMS_ADAPTER: AdapterSpec = {
  id: "terms-deadlock",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [{ kind: "terms", all: ["div.terms-modal"] }],
  workEvidence: { kind: "title", selector: "h1" },
  termsAccept: {
    modalSelector: "div.terms-modal",
    control: "button.accept",
    textAny: ["accept and download"],
  },
};

class FakePort implements NativePort {
  readonly posted: object[] = [];
  readonly onMessage = new FakeEmitter<[unknown]>();
  readonly onDisconnect = new FakeEmitter<[]>();
  private readonly waiters = new Set<(frame: BrowserMessage) => void>();

  postMessage(message: object): void {
    this.posted.push(message);
    const frame = parseBrowserMessage(message);
    for (const waiter of this.waiters) waiter(frame);
  }

  disconnect(): void {
    void this.onDisconnect.emit();
  }

  async inbound(message: unknown): Promise<void> {
    await this.onMessage.emit(message);
  }

  async waitForFrame(type: BrowserMessage["type"]): Promise<BrowserMessage> {
    const existing = this.posted
      .map(parseBrowserMessage)
      .find((frame) => frame.type === type);
    if (existing !== undefined) return existing;
    const { promise, resolve } = Promise.withResolvers<BrowserMessage>();
    const waiter = (frame: BrowserMessage): void => {
      if (frame.type !== type) return;
      this.waiters.delete(waiter);
      resolve(frame);
    };
    this.waiters.add(waiter);
    return promise;
  }
}

class FakeBackend implements StateBackend {
  private readonly termsAcknowledged = Promise.withResolvers<void>();

  constructor(public store: StoreShape) {}

  async load(): Promise<StoreShape> {
    return this.store;
  }

  async save(store: StoreShape): Promise<void> {
    this.store = store;
    if (
      Object.values(store.termsEffects ?? {}).some(
        (effect) => effect.acknowledged,
      )
    ) {
      this.termsAcknowledged.resolve();
    }
  }

  waitForTermsAcknowledgement(): Promise<void> {
    return this.termsAcknowledged.promise;
  }
}

function makeTermsPlan(): Plan {
  const window = new Window({ url: PROVIDER_URL });
  const title = window.document.createElement("h1");
  title.textContent = "Restored Terms Paper";
  window.document.body.appendChild(title);
  const modal = window.document.createElement("div");
  modal.className = "terms-modal";
  const accept = window.document.createElement("button");
  accept.className = "accept";
  accept.textContent = "Accept and download";
  modal.appendChild(accept);
  window.document.body.appendChild(modal);
  const plan = planExecution(
    window.document as unknown as Document,
    TERMS_ADAPTER,
    { title: "Restored Terms Paper" },
    { access_mode: "delegated" },
  );
  if ("assisted" in plan) {
    throw new Error(`invalid terms test plan: ${plan.assisted}`);
  }
  return plan;
}

function inbound(
  type: BrowserMessage["type"],
  payload: Record<string, unknown>,
  seq: number,
  jobID?: string,
): unknown {
  return {
    protocol: "papio-browser/1",
    type,
    msg_id: crypto.randomUUID().replaceAll("-", ""),
    ...(jobID === undefined ? {} : { job_id: jobID }),
    seq,
    payload,
  };
}

test("a restored terms job releases the inbound queue for its correlated reply", async () => {
  const port = new FakePort();
  const tabs = new ChromeTabsFake();
  tabs.seed({ id: TAB_ID, url: PROVIDER_URL, status: "complete" });
  const backend = new FakeBackend({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: JOB_ID,
        tab_id: TAB_ID,
        offered_at: 1_700_000_000_000,
        expires_at: Date.parse("2027-01-01T00:00:00Z"),
        status: "accepted",
        provider_hosts: [PROVIDER],
        access_mode: "delegated",
        expected: { title: "Restored Terms Paper" },
      },
    ],
  });
  const termsPlan = makeTermsPlan();
  const deps: BridgeDeps = {
    connectNative: () => port,
    manifestVersion: "0.1.0",
    randomUUID: () => crypto.randomUUID(),
    now: () => 1_700_000_000_000,
    setTimeout: () => {},
    backend,
    tabs,
    downloads: new FakeDownloads(),
    adapterSpecs: [TERMS_ADAPTER],
    scripting: {
      executeScript: async (injection) => {
        if (injection.func === assessDrivenPage)
          return [{ result: { kind: "normal" } }];
        if (injection.func === planExecution) return [{ result: termsPlan }];
        if (injection.func === executePlannedPageEffect)
          return [{ result: { ok: true } }];
        return [];
      },
    },
    permissions: { contains: async () => true },
    settings: {
      getTermsConsent: async () => "accept",
      setTermsConsent: async () => {},
      getHandoffSurface: async () => "in-window",
      getInPageToast: async () => false,
    },
    action: {
      setBadgeText: async () => {},
      setBadgeBackgroundColor: async () => {},
    },
    alarms: {
      create: () => {},
      onAlarm: new FakeEmitter<[{ name: string }]>(),
    },
  };
  const bridge = new Bridge(deps);
  await bridge.start();
  await port.inbound(
    inbound(
      "hello_ack",
      {
        daemon_version: MIN_DAEMON_VERSION,
        features: ["effect_permit_v1", "handoff_link_v1"],
        role: "holder",
        browser_holder_generation: 1,
      },
      1,
    ),
  );

  const offerHandled = port.inbound(
    inbound(
      "job_offer",
      {
        openurl: OPENURL,
        provider_hosts: [PROVIDER],
        expires_at: "2027-01-01T00:00:00Z",
        access_mode: "delegated",
        requires_auth: true,
        login_entity_id: "https://idp.example.edu/entity",
        expected: { title: "Restored Terms Paper" },
      },
      2,
      JOB_ID,
    ),
  );
  const startRequest = await port.waitForFrame("terms_effect_start_request");
  const startReplyHandled = port.inbound(
    inbound(
      "terms_effect_start_result",
      {
        request_id: startRequest.payload["request_id"],
        outcome: "started",
        permit_id: "permit_terms_deadlock_0001",
        terms_occurrence_id: "occurrence_terms_deadlock_0001",
      },
      3,
      JOB_ID,
    ),
  );

  const [, , resultRequest] = await Promise.all([
    offerHandled,
    startReplyHandled,
    port.waitForFrame("terms_effect_result_request"),
  ]);
  await port.inbound(
    inbound(
      "terms_effect_result",
      {
        request_id: resultRequest.payload["request_id"],
        permit_id: "permit_terms_deadlock_0001",
        terms_occurrence_id: "occurrence_terms_deadlock_0001",
        outcome: "applied",
      },
      4,
      JOB_ID,
    ),
  );

  await backend.waitForTermsAcknowledgement();
  expect(backend.store.termsEffects?.[JOB_ID]?.result_outcome).toBe("accepted");
  expect(backend.store.termsEffects?.[JOB_ID]?.acknowledged).toBe(true);
}, 500);
