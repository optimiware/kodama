import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseConfig } from "../src/config.ts";
import { createHost } from "../src/runtime/host.ts";
import { type ActiveInvocation, reportingPlugin } from "../src/runtime/plugin.ts";
import { SDKRuntime, sessionPermissions } from "../src/runtime/sdk.ts";

describe("embedded SDK host and session runtime", () => {
  test("starts embedded OpenCode host without HTTP port and enforces single database owner lock", async () => {
    const dir = mkdtempSync(join(tmpdir(), "openforge-sdk-"));
    const dbPath = join(dir, "opencode.db");
    try {
      const active = new Map<string, ActiveInvocation>();
      const host = await createHost({
        app: { name: "OpenForge", version: "1" },
        database: { path: dbPath },
        plugins: [reportingPlugin(active)],
      });
      try {
        await expect(
          createHost({
            app: { name: "OpenForge", version: "1" },
            database: { path: dbPath },
          }),
        ).rejects.toThrow("Another OpenForge host owns this execution database");

        const session = await host.sessions.create({
          agent: "implementer",
          model: { providerID: "opencode", id: "claude-sonnet-5-5" },
          location: { directory: dir },
          title: "test session",
          permissions: sessionPermissions({
            task: "T-001",
            role: "implementer",
            prompt: "hi",
            worktree: dir,
            owns: ["src/"],
            timeoutSeconds: 30,
          }),
        });
        expect(session.id).toMatch(/^ses/);
      } finally {
        await host.close();
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("SDKRuntime reuses existing sessionID when continuing from implementer to fixer", async () => {
    const active = new Map<string, ActiveInvocation>();
    const createdIds: string[] = [];
    const switchedRoles: string[] = [];

    const fakeHost = {
      sessions: {
        async create() {
          const id = `ses_${createdIds.length + 1}`;
          createdIds.push(id);
          return { id } as never;
        },
        async get({ sessionID }: { sessionID: string }) {
          if (!createdIds.includes(sessionID)) throw new Error("not found");
          return { id: sessionID } as never;
        },
        async switchAgent({ agent }: { sessionID: string; agent: string }) {
          switchedRoles.push(agent);
        },
        async switchModel() {},
        async update() {},
        async environment() {},
        async prompt({ sessionID }: { sessionID: string }) {
          const inv = active.get(sessionID);
          if (inv) {
            inv.result = { summary: "done", tests_run: ["make test"], blockers: [] };
          }
          return {} as never;
        },
        async wait() {},
        async interrupt() {
          return {} as never;
        },
      },
      async close() {},
    };

    const config = parseConfig({});
    const runtime = new SDKRuntime(fakeHost, active, {
      planner: config.roles.planner.model,
      implementer: config.roles.implementer.model,
      fixer: config.roles.fixer.model,
      reviewer: config.roles.reviewer.model,
    });

    const first = await runtime.run({
      task: "T-001",
      role: "implementer",
      prompt: "implement",
      worktree: "/tmp",
      owns: ["src/"],
      timeoutSeconds: 10,
    });
    expect(first.sessionID).toBe("ses_1");

    const second = await runtime.run({
      task: "T-001",
      role: "fixer",
      prompt: "fix",
      worktree: "/tmp",
      owns: ["src/"],
      sessionID: first.sessionID,
      timeoutSeconds: 10,
    });
    expect(second.sessionID).toBe("ses_1");
    expect(createdIds).toEqual(["ses_1"]);
    expect(switchedRoles).toEqual(["fixer"]);
  });
});
