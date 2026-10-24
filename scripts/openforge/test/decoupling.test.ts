import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import { loadConfig } from "../src/config.ts";
import { loadTasks, validateTasks } from "../src/tasks.ts";

const SRC_DIR = resolve(import.meta.dir, "../src");
const REPO_ROOT = resolve(import.meta.dir, "../../..");

function walkTsFiles(dir: string): string[] {
  const results: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const st = statSync(full);
    if (st.isDirectory()) results.push(...walkTsFiles(full));
    else if (entry.endsWith(".ts")) results.push(full);
  }
  return results;
}

describe("harness decoupling and repository contract", () => {
  test("openforge source files contain zero Kodama, Go, or Podman references", () => {
    const forbidden =
      /\bkodama\b|go\.mod|go\.sum|\bgo test\b|\bgo vet\b|\bGO_CORE|\bpodman\b|\bDEV_SCOPE\b|\bDEV_PROVIDER\b|host-env\.json|scripts\/dev|tenant-authority/i;
    const files = walkTsFiles(SRC_DIR);
    expect(files.length).toBeGreaterThan(5);
    const violations: string[] = [];
    for (const file of files) {
      const content = readFileSync(file, "utf8");
      const match = forbidden.exec(content);
      if (match) {
        violations.push(`${file}: matched "${match[0]}"`);
      }
    }
    expect(violations).toEqual([]);
  });

  test("repository openforge.json and task graph validate cleanly", () => {
    const config = loadConfig(REPO_ROOT);
    const tasks = loadTasks(REPO_ROOT, config.tasks);
    expect(tasks.size).toBeGreaterThan(0);
    const errors = validateTasks(tasks, join(REPO_ROOT, config.index), config);
    expect(errors).toEqual([]);
  });
});
