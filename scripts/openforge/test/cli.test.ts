import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { main } from "../src/cli.ts";
import { parseConfig } from "../src/config.ts";
import { git } from "../src/system.ts";
import { loadTasks, READY_END, READY_START, writeIndex } from "../src/tasks.ts";

async function createCliRepo() {
  const root = mkdtempSync(join(tmpdir(), "openforge-cli-"));
  await git(root, "init", "-b", "main");
  await git(root, "config", "user.name", "Tester");
  await git(root, "config", "user.email", "tester@example.com");
  mkdirSync(join(root, "docs/tasks"), { recursive: true });
  writeFileSync(
    join(root, "docs/tasks/T-001-demo.md"),
    `---
id: T-001
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [src/]
check: printf '{"passed":["TestDemo"],"failed":[],"skipped":[]}' > "$FORGE_RESULTS"
acceptance: [demo]
---
# T-001 — Demo task
`,
  );
  const readme = join(root, "docs/tasks/README.md");
  writeFileSync(
    readme,
    `# Tasks\n\n${READY_START}\n${READY_END}\n\n## Dependency index\n\n| Task | Dependencies | Status / decision |\n| --- | --- | --- |\n`,
  );
  const config = parseConfig({ mainCheck: "true" });
  writeIndex(loadTasks(root), readme, false, config);
  writeFileSync(join(root, "openforge.json"), JSON.stringify(config, null, 2));
  await git(root, "add", "-A");
  await git(root, "commit", "-m", "init");
  return root;
}

describe("CLI commands", () => {
  test("validate, ready, index, status, check-task, batch, and run --dry-run succeed", async () => {
    const root = await createCliRepo();
    try {
      expect(await main(["validate"], root)).toBe(0);
      expect(await main(["ready"], root)).toBe(0);
      expect(await main(["index"], root)).toBe(0);
      expect(await main(["status", "--json"], root)).toBe(0);
      expect(await main(["check-task", "T-001"], root)).toBe(0);
      expect(await main(["batch", "--run"], root)).toBe(0);
      expect(await main(["batch"], root)).toBe(0);
      expect(await main(["run", "--dry-run", "--only", "T-001"], root)).toBe(0);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
