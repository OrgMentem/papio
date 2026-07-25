// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Shared acquisition-stats presentation logic for the popup summary and the
// history page: one estimate constant, one reply parser, and one formatting
// vocabulary, so both surfaces always tell the same story.

import type { StatsAccess, StatsBucket, StatsResponsePayload } from "./protocol";

/** The stats reply as the background broker relays it (request_id stripped). */
export type AcquisitionStats = Omit<StatsResponsePayload, "request_id">;

/** Manually chasing one paywalled paper — resolver hops, login walls, and
 * downloading and filing the PDF — takes roughly 20 minutes. The "time saved"
 * headline multiplies this rough estimate per acquired paper; the daemon
 * reports counts only and never a time figure. */
export const EST_MINUTES_SAVED_PER_PAPER = 20;

export type StatsReply =
  | { ok: true; stats: AcquisitionStats }
  | { ok: false; code: string; message: string };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isCount(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0;
}

function isAccess(value: unknown): value is StatsAccess {
  return (
    isRecord(value) &&
    isCount(value["open_access"]) &&
    isCount(value["institutional"]) &&
    isCount(value["licensed_api"]) &&
    isCount(value["other"])
  );
}

function isBucket(value: unknown): value is StatsBucket {
  return isRecord(value) && typeof value["period_start"] === "string" && isCount(value["acquired"]);
}

/**
 * Narrow a raw `papio.stats` runtime reply. Anything that is not a fully
 * well-formed success — a broker failure, a transport hiccup, or a malformed
 * payload — collapses into `{ok:false}` so callers render one muted
 * "stats unavailable" state instead of a hard error.
 */
export function parseStatsReply(value: unknown): StatsReply {
  if (isRecord(value) && value["ok"] === true && isRecord(value["stats"])) {
    const stats = value["stats"];
    const series = stats["series"];
    if (
      typeof stats["generated_at"] === "string" &&
      isCount(stats["acquired_total"]) &&
      isCount(stats["failed_total"]) &&
      isCount(stats["handoffs_required"]) &&
      isAccess(stats["access"]) &&
      Array.isArray(series) &&
      series.every(isBucket)
    ) {
      return { ok: true, stats: stats as unknown as AcquisitionStats };
    }
    return { ok: false, code: "invalid_reply", message: "The daemon returned an unusable stats reply." };
  }
  const error = isRecord(value) ? value["error"] : undefined;
  return {
    ok: false,
    code: isRecord(error) && typeof error["code"] === "string" ? error["code"] : "unavailable",
    message:
      isRecord(error) && typeof error["message"] === "string"
        ? error["message"]
        : "The daemon did not return a usable response.",
  };
}

/** "14 h" for 840 minutes; small figures keep one decimal ("0.7 h"). */
export function formatHoursSaved(minutes: number): string {
  const hours = minutes / 60;
  const rounded = hours >= 10 ? Math.round(hours) : Math.round(hours * 10) / 10;
  return `${rounded} h`;
}

/** "75%" — or an em dash when the denominator is zero. */
export function formatShare(numerator: number, denominator: number): string {
  if (denominator <= 0) return "—";
  return `${Math.round((numerator / denominator) * 100)}%`;
}
