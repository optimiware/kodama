import { closeSync, existsSync, openSync, readSync, statSync } from "node:fs";
import { join } from "node:path";

export interface FactoryEvent {
  type?: string;
  task?: string;
  reason?: string;
  [key: string]: unknown;
}

export function readEvents(primary: string, byteOffset: number): { events: FactoryEvent[]; next: number } {
  const path = join(primary, ".work", ".harness", "events.jsonl");
  if (!existsSync(path)) return { events: [], next: 0 };
  const size = statSync(path).size;
  const start = byteOffset > size ? 0 : byteOffset;
  if (size === start) return { events: [], next: start };
  const fd = openSync(path, "r");
  try {
    const buffer = Buffer.alloc(size - start);
    readSync(fd, buffer, 0, buffer.length, start);
    const text = buffer.toString("utf8");
    const end = text.endsWith("\n") ? text.length : text.lastIndexOf("\n") + 1;
    if (end <= 0) return { events: [], next: start };
    const events: FactoryEvent[] = [];
    for (const line of text.slice(0, end).split("\n")) {
      if (!line) continue;
      try {
        events.push(JSON.parse(line) as FactoryEvent);
      } catch {
        // Skip a corrupt line rather than stalling the cursor.
      }
    }
    return { events, next: start + Buffer.byteLength(text.slice(0, end)) };
  } finally {
    closeSync(fd);
  }
}

const NOTIFY = new Set([
  "parked",
  "paused",
  "merged",
  "queue_started",
  "queue_halt_requested",
  "queue_resumed",
  "main_check_started",
  "operator_published",
]);

export function formatEvent(event: FactoryEvent): string | null {
  if (!event.type || !NOTIFY.has(event.type)) return null;
  const task = event.task && event.task !== "-" ? ` ${event.task}` : "";
  const reason = typeof event.reason === "string" ? `: ${event.reason}` : "";
  return `${event.type}${task}${reason}`.slice(0, 800);
}
