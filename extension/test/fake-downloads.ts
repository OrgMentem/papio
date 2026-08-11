import type { DownloadDeltaLike, DownloadItemLike } from "../src/background";
import { FakeEmitter } from "./fake-tabs";

export class FakeDownloads {
  readonly onCreated = new FakeEmitter<[DownloadItemLike]>();
  readonly onChanged = new FakeEmitter<[DownloadDeltaLike]>();
  readonly onDeterminingFilename = new FakeEmitter<
    [DownloadItemLike, (s: { filename: string; conflictAction: "uniquify" }) => void]
  >();
  readonly items = new Map<number, DownloadItemLike>();
  readonly started: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }[] = [];
  readonly removedFiles: number[] = [];
  readonly erased: number[] = [];
  /** Optional hook runs after Chrome has created its durable item but before
   * downloads.download resolves with the item ID. Tests use it to start a
   * second Bridge, modelling an MV3 worker death in that exact window. */
  afterCreate?: (downloadID: number) => void | Promise<void>;
  failDownload = false;

  async download(options: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }): Promise<number> {
    this.started.push(options);
    if (this.failDownload) throw new Error("download blocked");
    const id = 900 + this.started.length;
    this.items.set(id, { id, url: options.url, filename: options.filename, state: "in_progress" });
    await this.afterCreate?.(id);
    return id;
  }

  async removeFile(downloadID: number): Promise<void> {
    this.removedFiles.push(downloadID);
  }

  async erase(query: { id: number }): Promise<number[]> {
    this.erased.push(query.id);
    return [query.id];
  }

  async search(query: { id?: number; filename?: string; limit?: number }): Promise<DownloadItemLike[]> {
    let result = [...this.items.values()];
    if (query.id !== undefined) result = result.filter((item) => item.id === query.id);
    if (query.filename !== undefined) result = result.filter((item) => item.filename?.includes(query.filename ?? "") === true);
    return query.limit === undefined ? result : result.slice(0, query.limit);
  }
}
