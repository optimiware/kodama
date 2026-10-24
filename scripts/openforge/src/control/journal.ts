import { join } from "node:path";
import { readJSON, withLock, writeJSON } from "../system.ts";

export interface JournalEntry {
  status: "running" | "done" | "failed";
  result?: string;
  error?: string;
}

const journalPath = (primary: string) => join(primary, ".work", ".harness", "operator", "operations.json");

export function loadJournal(primary: string): Record<string, JournalEntry> {
  return readJSON<Record<string, JournalEntry>>(journalPath(primary), {});
}

export async function once(
  primary: string,
  id: string,
  fn: () => Promise<string>,
  options: { retryFailed?: boolean } = {},
): Promise<{ repeated: boolean; result: string }> {
  const lock = join(primary, ".work", ".harness", "operator", "operations.lock");
  return withLock(lock, async () => {
    const journal = loadJournal(primary);
    const current = journal[id];
    if (current?.status === "done" && current.result !== undefined) {
      return { repeated: true, result: current.result };
    }
    if (current?.status === "running") {
      throw new Error(`Operation ${id} is already in progress`);
    }
    if (current?.status === "failed" && !options.retryFailed) {
      throw new Error(current.error ?? `Operation ${id} previously failed`);
    }
    journal[id] = { status: "running" };
    writeJSON(journalPath(primary), journal);
    try {
      const result = await fn();
      journal[id] = { status: "done", result };
      writeJSON(journalPath(primary), journal);
      return { repeated: false, result };
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      journal[id] = { status: "failed", error: message };
      writeJSON(journalPath(primary), journal);
      throw error;
    }
  });
}
