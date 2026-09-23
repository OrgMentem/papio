// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import type { NativeViewerSaveReason } from "./protocol";

/** Recovery evidence only. Never contains a URL, document token, or replayable request. */
export interface NativeViewerLatch {
  token: string;
  action_id: number;
  action_revision: number;
  state: "running" | "interrupted" | "settled" | "retryable";
}

export function migrateNativeViewerLatch(value: unknown): NativeViewerLatch | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const v = value as Record<string, unknown>;
  if (typeof v.token !== "string" || !/^[a-zA-Z0-9_-]{1,128}$/.test(v.token) ||
      !Number.isSafeInteger(v.action_id) || (v.action_id as number) < 1 ||
      !Number.isSafeInteger(v.action_revision) || (v.action_revision as number) < 1 ||
      !["running", "interrupted", "settled", "retryable"].includes(String(v.state))) return undefined;
  return { token: v.token, action_id: v.action_id as number,
    action_revision: v.action_revision as number, state: v.state as NativeViewerLatch["state"] };
}

/** Diagnosis is structured daemon data, never inferred from human-readable Detail. */
export function nativeViewerAction(snapshot: Record<string, unknown>, jobID: string, automatic: boolean):
  { action_id: number; action_revision: number } | undefined {
  if (!Array.isArray(snapshot.items)) return undefined;
  const matches = snapshot.items.filter((item): item is Record<string, unknown> =>
    typeof item === "object" && item !== null && item.kind === "human_action" &&
    item.job_id === jobID && item.job_state === "awaiting_human" &&
    (item.route_class ?? item.action_kind) === "manual_download");
  if (matches.length !== 1) return undefined;
  const item = matches[0]!;
  if (!Number.isSafeInteger(item.action_id) || (item.action_id as number) < 1 ||
      !Number.isSafeInteger(item.revision) || (item.revision as number) < 1) return undefined;
  if (automatic && (!Array.isArray(item.facts) || !item.facts.some(fact =>
    fact?.label === "Diagnosis" && fact?.text === "native_viewer_download_required"))) return undefined;
  return { action_id: item.action_id as number, action_revision: item.revision as number };
}

export const NATIVE_VIEWER_INTERRUPTED = "The viewer save was interrupted. Check this paper in papio before trying another save; papio will not repeat it automatically.";
export function nativeViewerReason(reason: NativeViewerSaveReason | "interrupted" | undefined): string {
  switch (reason) {
    case "document_changed": return "The PDF tab changed or is no longer uniquely active. Select the original viewer and check this paper in papio.";
    case "unsupported": return "This Firefox cannot identify the live PDF document. Update to Firefox 153 or newer before using native viewer save.";
    case "authority_lost": return "The paper, action, or browser session changed. Check the current action in papio.";
    case "source_rejected": return "The native helper could not bind this PDF viewer. Select the original PDF tab and try Send this PDF again.";
    case "source_busy": return "Another save owns this PDF viewer. Wait for it to finish and check the paper in papio.";
    case "already_started": return "A save already started for this paper. Check its result in papio; it will not be repeated.";
    case "invalid_pdf": return "Papio rejected the saved file. Check the paper's current action in papio.";
    case "validation_pending": return "The saved file needs review in papio.";
    case "native_failed": return "Firefox could not finish the native save. Check its save dialog and the paper in papio.";
    case "unavailable": return "Native viewer save is unavailable. Check the native helper and the current action in papio.";
    default: return NATIVE_VIEWER_INTERRUPTED;
  }
}
