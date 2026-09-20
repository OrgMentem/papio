// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Developer fixture plumbing only; this is not an acquisition authority API.
import type { Bridge } from "../src/background";

export interface PageSpikeConfig {
  readonly controllerPath: string;
  readonly pdfURL: string;
  readonly jobID: string;
}

export type PageSpikeDeliveryFence = Pick<PageSpikeConfig, "pdfURL" | "jobID">;
type Request = Parameters<Bridge["startPDFDelivery"]>[0];
type Reply = Awaited<ReturnType<Bridge["startPDFDelivery"]>>;
export const PAGE_SPIKE_DELIVERY = "papio.page_spike.delivery";
const uuid = "[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}";
const controllerPattern = new RegExp(`^dist/page-spike-(${uuid})/run\\.html$`);

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** Shared by the build and worker: reject alternate origins, URL normalization,
 * queries, traversal, extra authority and mismatched fixture nonces. */
export function parsePageSpikeConfig(value: unknown): PageSpikeConfig {
  if (!record(value) || Object.keys(value).sort().join(",") !== "controllerPath,jobID,pdfURL" ||
      typeof value.controllerPath !== "string" || typeof value.pdfURL !== "string" ||
      typeof value.jobID !== "string" || !/^job_[0-9a-f]{24,64}$/.test(value.jobID)) {
    throw new Error("Invalid PAPIO_PAGE_SPIKE_CONFIG shape");
  }
  const match = controllerPattern.exec(value.controllerPath);
  const pdfPattern = new RegExp(`^http://127\\.0\\.0\\.1:([1-9][0-9]{0,4})/${match?.[1]}/fixture/[A-Za-z0-9][A-Za-z0-9_-]{0,127}\\.pdf$`);
  const pdfMatch = pdfPattern.exec(value.pdfURL);
  if (!match || !pdfMatch || Number(pdfMatch[1]) > 65535 || new URL(value.pdfURL).href !== value.pdfURL) {
    throw new Error("Page spike must name one canonical loopback PDF and its controller");
  }
  return Object.freeze({ controllerPath: value.controllerPath, pdfURL: value.pdfURL, jobID: value.jobID });
}

/** A release build cannot accidentally inherit an experiment from its shell. */
export function pageSpikeBuildConfig(value: unknown, daemonVersion: string): PageSpikeConfig | null {
  if (value === null) return null;
  if (!/^\d+\.\d+\.\d+-dev(?:[.+-][A-Za-z0-9.-]+)?$/.test(daemonVersion)) {
    throw new Error("PAPIO_PAGE_SPIKE_CONFIG requires a development build");
  }
  return parsePageSpikeConfig(value);
}

interface Dependencies {
  runtimeID: string;
  getSelf(): Promise<{ id: string; installType: string }>;
  getTab(tabID: number): Promise<{ url?: string | undefined }>;
  startPDFDelivery(request: Request, fence: PageSpikeDeliveryFence): Promise<Reply>;
}

function failure(code: string, message: string): Reply {
  return { ok: false, error: { code, message } };
}

function normalizeReply(reply: Reply): Reply {
  if (reply.ok || "error" in reply) return reply;
  // The existing expired-choice branch predates the broker error envelope.
  const legacy = reply as { code?: unknown; message?: unknown };
  return failure(
    typeof legacy.code === "string" ? legacy.code : "delivery_refused",
    typeof legacy.message === "string" ? legacy.message : "Bridge refused the fixture delivery",
  );
}

/** No listener is enabled by a normal build. One authorized dispatch per worker;
 * existing Bridge pending-delivery state also fences worker-restart retries.
 * This module never writes storage, invents jobs or calls downloads.download. */
export function createPageSpikeDeliveryHandler(rawConfig: unknown, deps: Dependencies) {
  if (rawConfig === null) return undefined;
  let config: PageSpikeConfig;
  try { config = parsePageSpikeConfig(rawConfig); }
  catch { return async () => failure("invalid_config", "Invalid page fixture configuration"); }
  let dispatched = false;
  return async (message: unknown, sender: { id?: string | undefined; url?: string | undefined }): Promise<Reply> => {
    if (!/^[a-p]{32}$/.test(deps.runtimeID) || sender.id !== deps.runtimeID ||
        sender.url !== `chrome-extension://${deps.runtimeID}/${config.controllerPath}`) {
      return failure("unauthorized", "Only the configured fixture controller may send this request");
    }
    if (!record(message) || Object.keys(message).sort().join(",") !== "tab_id,type" ||
        message.type !== PAGE_SPIKE_DELIVERY || typeof message.tab_id !== "number" ||
        !Number.isSafeInteger(message.tab_id) || message.tab_id < 0) {
      return failure("invalid_request", "Expected one fixture tab ID");
    }
    if (dispatched) return failure("duplicate_dispatch", "This fixture delivery was already requested");
    // Latch before the first await; errors do not make an uncertain effect retryable.
    dispatched = true;
    try {
      const install = await deps.getSelf();
      if (install.id !== deps.runtimeID || install.installType !== "development") {
        return failure("not_development", "Fixture delivery requires an unpacked development extension");
      }
      if ((await deps.getTab(message.tab_id)).url !== config.pdfURL) {
        return failure("fixture_changed", "The current tab is not the configured fixture PDF");
      }
      const request = { tab_id: message.tab_id, url: config.pdfURL };
      const reply = normalizeReply(await deps.startPDFDelivery(request, config));
      if (!reply.ok) return reply;
      if (reply.state !== "needs_choice" || !reply.choice ||
          !reply.choice.candidates.some((candidate) => candidate.job_id === config.jobID)) {
        return failure("job_mismatch", "The fixture job was not offered for PDF delivery");
      }
      // Bridge consumes its existing one-use choice and revalidates the live
      // document identity. The fence prevents automatic resolution beforehand.
      const delivered = normalizeReply(await deps.startPDFDelivery({ ...request, choice: {
        interaction: reply.choice.interaction, job_id: config.jobID,
      } }, config));
      if (delivered.ok && delivered.job_id !== config.jobID) {
        return failure("job_mismatch", "PDF delivery returned an unexpected job");
      }
      return delivered;
    } catch {
      return failure("fixture_delivery_failed", "Fixture delivery could not be completed; inspect the job before retrying");
    }
  };
}
