import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseConfig } from "../src/config.ts";
import {
  claimable,
  findCycle,
  loadTasks,
  READY_END,
  READY_START,
  ready,
  validateTasks,
  writeIndex,
} from "../src/tasks.ts";

function makeTempRepo(): string {
  const root = mkdtempSync(join(tmpdir(), "openforge-tasks-"));
  mkdirSync(join(root, "docs/tasks"), { recursive: true });
  return root;
}

function writeTask(root: string, filename: string, content: string): void {
  writeFileSync(join(root, "docs/tasks", filename), content);
}

describe("task graph and index", () => {
  test("parses frontmatter, orders ready tasks by critical path, and selects disjoint claimable tasks", () => {
    const root = makeTempRepo();
    try {
      const config = parseConfig({ resources: { cpu: 2 } });
      writeTask(
        root,
        "T-001-base.md",
        `---
id: T-001
status: done
depends_on: []
owns: [src/base/]
---
# T-001 — Base
`,
      );
      writeTask(
        root,
        "T-002-mid.md",
        `---
id: T-002
status: planned
depends_on: [T-001]
owns: [src/a/]
check: make test-a
resources: [cpu]
acceptance: [test_a]
---
# T-002 — Mid
`,
      );
      writeTask(
        root,
        "T-003-leaf.md",
        `---
id: T-003
status: planned
depends_on: [T-002]
owns: [src/b/]
---
# T-003 — Leaf
`,
      );
      writeTask(
        root,
        "T-004-parallel.md",
        `---
id: T-004
status: planned
depends_on: [T-001]
owns: [src/a/sub/]
---
# T-004 — Overlapping with T-002
`,
      );
      const readmePath = join(root, "docs/tasks/README.md");
      writeFileSync(
        readmePath,
        `# Tasks\n\n${READY_START}\n${READY_END}\n\n## Dependency index\n\n| Task | Dependencies | Status / decision |\n| --- | --- | --- |\n`,
      );

      const tasks = loadTasks(root);
      writeIndex(tasks, readmePath, false, config);

      const readyIds = ready(tasks, config).map((t) => t.id);
      // T-002 has downstream T-003 so it ranks ahead of T-004
      expect(readyIds).toEqual(["T-002", "T-004"]);

      // T-002 and T-004 overlap on src/a/, so claimable only picks T-002
      const claimed = claimable(tasks, 5, new Set(), config).map((t) => t.id);
      expect(claimed).toEqual(["T-002"]);

      // validateTasks catches the unordered owns conflict between T-002 and T-004
      const errors = validateTasks(tasks, readmePath, config);
      expect(errors.some((e) => e.includes("concurrent owns conflict"))).toBe(true);

      const readme = readFileSync(readmePath, "utf8");
      expect(readme).toContain("[T-002](T-002-mid.md)");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("detects dependency cycles and invalid frontmatter keys or resources", () => {
    const root = makeTempRepo();
    try {
      const config = parseConfig({ resources: { db: 1 } });
      writeTask(
        root,
        "T-001-a.md",
        `---
id: T-001
status: planned
depends_on: [T-002]
owns: [a/]
resources: [unknown_res]
unknown_field: foo
---
# T-001 — A
`,
      );
      writeTask(
        root,
        "T-002-b.md",
        `---
id: T-002
status: planned
depends_on: [T-001]
owns: [../escape]
---
# T-002 — B
`,
      );
      const tasks = loadTasks(root);
      expect(findCycle(tasks)).toEqual(["T-001", "T-002", "T-001"]);
      const errors = validateTasks(tasks, undefined, config);
      expect(errors.some((e) => e.includes("dependency cycle"))).toBe(true);
      expect(errors.some((e) => e.includes("unknown resource unknown_res"))).toBe(true);
      expect(errors.some((e) => e.includes("unknown frontmatter keys: unknown_field"))).toBe(true);
      expect(errors.some((e) => e.includes("owned path escapes checkout"))).toBe(true);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
