/**
 * Firefox capture of a signed PDF viewer's one response, only in the handoff
 * tabs papio arms for a job. The Firefox counterpart of Chrome's session rules
 * in viewer-download-rule.ts.
 *
 * A signed viewer URL is not reusable (see viewer-download-rule.ts), so the
 * PDF exists only in the response the browser already received. Firefox has
 * no declarativeNetRequest response-header condition, and it cannot steer a
 * download it did not start (no `onDeterminingFilename`). So papio tees that
 * response with a StreamFilter: every chunk goes on to the viewer unchanged,
 * and papio keeps a copy. A complete copy is then saved with
 * `downloads.download` from an object URL, which Firefox files under
 * `papio/<job>/` like any other extension-started download. The daemon's
 * adoption, validation and identity checks run as for every other file.
 *
 * papio never requests the signed URL a second time. A failed or incomplete
 * copy is reported through the viewer Download notice instead.
 *
 * Measured on Firefox 156 with loopback fixtures (2026-09-24):
 * - the filter works from a blocking `onHeadersReceived` listener registered
 *   in the MV3 event page's first turn, including on a page Firefox woke for
 *   the response, and when the listener's promise first awaits storage;
 * - `write` does not detach the chunk, so the copy keeps it without copying;
 * - with `Content-Encoding: gzip` the filter receives the DECODED body, so the
 *   wire Content-Length is compared only for an unencoded body;
 * - for a slow 40 MB PDF, pdf.js sends its own Range requests on separate
 *   channels (a tail request, or the whole file where the server serves 206),
 *   yet the original response still runs to completion through the filter,
 *   even when those Range requests get HTML.
 *
 * Firefox's viewer can be a top-level tab, an <iframe> (`sub_frame`), or an
 * <object> or <embed> (`object`), and the listener takes all three. On
 * 2026-09-24 a ScienceDirect article reached through a Cloudflare check
 * rendered its PDF in a pdf.js viewer inside the page, not as a top-level
 * tab; which element held it was not recorded. A PDF that a page script
 * fetches itself (`xmlhttprequest`) stays outside the capture: a blocking
 * listener on every script request would wake the event page for each one,
 * in every tab.
 */
import { matchesViewerDownloadRule } from "./viewer-download-rule";

/** The daemon's default `fetch.max_bytes`. The extension does not receive a
 * job's fetch policy, and the daemon still enforces its own limit on
 * adoption; this bounds only what the capture holds in memory. */
export const VIEWER_CAPTURE_MAX_BYTES = 100 * 1024 * 1024;

/** `%PDF-` must lie within this many leading bytes, as PDF readers allow. */
const PDF_HEADER_WINDOW = 1024;
const PDF_CONTENT_TYPES: Record<string, true> = { "application/pdf": true, "application/x-pdf": true };

/** The subset of Firefox's StreamFilter the capture uses. */
export interface StreamFilterLike {
  ondata: ((event: { data: ArrayBuffer }) => void) | null;
  onstop: ((event: unknown) => void) | null;
  onerror: ((event: unknown) => void) | null;
  readonly error: string;
  write(data: ArrayBuffer): void;
  close(): void;
  /** Hand the rest of the response back to the browser, unfiltered. */
  disconnect(): void;
}

export interface ViewerHeadersDetails {
  requestId: string;
  url: string;
  tabId: number;
  statusCode: number;
  responseHeaders?: { name: string; value?: string }[];
}

/** Firefox's `webRequest` seam (see BridgeDeps.webRequest). */
export interface ViewerCaptureWebRequest {
  onHeadersReceived: {
    addListener(
      callback: (details: ViewerHeadersDetails) => Record<string, never> | Promise<Record<string, never>>,
      filter: { urls: string[]; types: ("main_frame" | "sub_frame" | "object")[] },
      extraInfoSpec: ("blocking" | "responseHeaders")[],
    ): void;
  };
  filterResponseData(requestId: string): StreamFilterLike;
}

/** A complete copy of one signed viewer response. */
export interface CapturedViewerPDF {
  jobID: string;
  tabID: number;
  url: string;
  body: Blob;
}

/** The Bridge state and behaviour the capture depends on. */
export interface ViewerCaptureContext {
  /** Undefined once managed state is loaded; until then the armed set is
   * unknown, so a response that woke the event page waits for it. */
  hydrating(): Promise<void> | undefined;
  /** The job whose armed tab this is, if any (ViewerRuleSync.armedJob). */
  armedJob(tabID: number): string | undefined;
  /** The armed set changed: this job has spent its one attempt. */
  armedChanged(): void;
  /** Save a complete copy. The Bridge settles the attempt with `settle`. */
  save(capture: CapturedViewerPDF): void;
  /** No usable copy: ask for the viewer's Download button, naming why. */
  miss(jobID: string, url: string, tabID: number, detail: string): void;
}

type CapturePhase = "streaming" | "saving" | "saved" | "failed";

/** Captures a signed viewer response in an armed Firefox tab. One attempt per
 * job: the first matching response spends it, whatever its outcome. */
export class ViewerStreamCapture {
  /** Job id -> its one capture attempt. Memory-only; the bytes and URL never
   * leave the extension except as the saved file. */
  private readonly attempts = new Map<string, CapturePhase>();
  private bound = false;

  constructor(
    private readonly deps: { webRequest?: ViewerCaptureWebRequest },
    private readonly ctx: ViewerCaptureContext,
  ) {}

  available(): boolean {
    return this.deps.webRequest !== undefined;
  }

  /** Register the blocking listener. The caller runs this synchronously in
   * the event page's first turn, so Firefox wakes a suspended page for it. */
  bind(): void {
    const webRequest = this.deps.webRequest;
    if (webRequest === undefined || this.bound) return;
    this.bound = true;
    webRequest.onHeadersReceived.addListener(
      (details) => this.onHeadersReceived(details),
      { urls: ["https://*/*"], types: ["main_frame", "sub_frame", "object"] },
      ["blocking", "responseHeaders"],
    );
  }

  /** The job has used its one capture attempt. */
  attempted(jobID: string): boolean {
    return this.attempts.has(jobID);
  }

  /** A capture in progress or saved owns the job's signed viewer. */
  owns(jobID: string): boolean {
    const phase = this.attempts.get(jobID);
    return phase === "streaming" || phase === "saving" || phase === "saved";
  }

  /** The Bridge's outcome for a copy it was asked to save. */
  settle(jobID: string, outcome: "saved" | "failed"): void {
    if (this.attempts.get(jobID) === "saving") this.attempts.set(jobID, outcome);
  }

  private onHeadersReceived(details: ViewerHeadersDetails): Record<string, never> | Promise<Record<string, never>> {
    if (details.tabId < 0 || details.statusCode !== 200 || !matchesViewerDownloadRule(details.url)) return {};
    if (PDF_CONTENT_TYPES[header(details, "content-type")?.split(";", 1)[0]?.trim().toLowerCase() ?? ""] !== true) return {};
    const hydrating = this.ctx.hydrating();
    if (hydrating === undefined) {
      this.capture(details);
      return {};
    }
    // Firefox holds the response while a blocking listener's promise is
    // pending, so the filter still sees the first byte.
    return hydrating.then(
      () => {
        this.capture(details);
        return {};
      },
      () => ({}),
    );
  }

  private capture(details: ViewerHeadersDetails): void {
    const tabID = details.tabId;
    const jobID = this.ctx.armedJob(tabID);
    if (jobID === undefined || this.attempts.has(jobID)) return;
    const url = details.url;
    let filter: StreamFilterLike;
    try {
      filter = this.deps.webRequest!.filterResponseData(details.requestId);
    } catch {
      return;
    }
    this.attempts.set(jobID, "streaming");
    this.ctx.armedChanged();
    const encoding = header(details, "content-encoding")?.trim().toLowerCase();
    const lengthHeader = header(details, "content-length")?.trim();
    // A decoded body differs from the wire length, so only an unencoded
    // body can be checked against Content-Length.
    const expected = (encoding === undefined || encoding === "" || encoding === "identity") &&
        lengthHeader !== undefined && /^\d{1,15}$/u.test(lengthHeader)
      ? Number(lengthHeader)
      : undefined;
    let chunks: ArrayBuffer[] = [];
    let received = 0;
    let overCap = false;
    let settled = false;
    const fail = (detail: string): void => {
      if (settled) return;
      settled = true;
      chunks = [];
      this.attempts.set(jobID, "failed");
      this.ctx.miss(jobID, url, tabID, detail);
    };
    filter.ondata = (event) => {
      // The viewer gets every byte first and unchanged; the copy is secondary.
      filter.write(event.data);
      received += event.data.byteLength;
      if (overCap) return;
      if (received > VIEWER_CAPTURE_MAX_BYTES) {
        overCap = true;
        chunks = [];
        return;
      }
      chunks.push(event.data);
    };
    filter.onerror = () => {
      fail("the signed viewer response failed before papio had the whole PDF");
      // Never keep owning a response that errored: whatever the browser still
      // delivers goes to the viewer without papio in the way.
      try {
        filter.disconnect();
      } catch {
        // An errored filter may already be detached.
      }
    };
    filter.onstop = () => {
      try {
        filter.close();
      } catch {
        // The viewer already has the bytes; a failed close changes nothing here.
      }
      if (settled) return;
      if (overCap) return fail("the signed viewer PDF is larger than papio's capture limit");
      if (expected !== undefined && received !== expected)
        return fail(`the signed viewer response ended after ${received} of ${expected} bytes`);
      if (!startsAsPDF(chunks)) return fail("the signed viewer response was not a PDF");
      settled = true;
      const body = new Blob(chunks, { type: "application/pdf" });
      chunks = [];
      this.attempts.set(jobID, "saving");
      this.ctx.save({ jobID, tabID, url, body });
    };
  }
}

function header(details: ViewerHeadersDetails, name: string): string | undefined {
  return details.responseHeaders?.find((candidate) => candidate.name.toLowerCase() === name)?.value;
}

/** True when `%PDF-` lies within the first PDF_HEADER_WINDOW bytes. */
function startsAsPDF(chunks: readonly ArrayBuffer[]): boolean {
  let head = "";
  for (const chunk of chunks) {
    if (head.length >= PDF_HEADER_WINDOW) break;
    head += String.fromCharCode(...new Uint8Array(chunk, 0, Math.min(chunk.byteLength, PDF_HEADER_WINDOW - head.length)));
  }
  return head.includes("%PDF-");
}
