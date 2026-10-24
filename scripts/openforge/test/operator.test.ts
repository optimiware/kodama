import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseConfig } from "../src/config.ts";
import { prepareTaskChange, publishChange } from "../src/control/changes.ts";
import { nextDecisionId, nextTaskId, upsertDecisionRow } from "../src/control/format.ts";
import { once } from "../src/control/journal.ts";
import {
  haltQueue,
  type ProcessLauncher,
  queueLiveness,
  resumeQueue,
  unparkOnce,
} from "../src/control/queue.ts";
import { loadDotenv, parseDotenv, redactSecrets } from "../src/dotenv.ts";
import { operatorPermissions } from "../src/operator/plugin.ts";
import type { ConversationalRuntime } from "../src/operator/runtime.ts";
import { drainOnce, loadOperatorState, type OperatorService } from "../src/operator/service.ts";
import { credentialsFromEnv, providerCredentials } from "../src/runtime/credentials.ts";
import { git, writeJSON } from "../src/system.ts";
import { loadTasks, READY_END, READY_START, writeIndex } from "../src/tasks.ts";
import { authorize } from "../src/telegram/access.ts";
import {
  splitText,
  TelegramClient,
  TelegramError,
  type TelegramTransport,
  type TelegramUpdate,
} from "../src/telegram/client.ts";

describe("dotenv and provider credentials", () => {
  test("parses quoted values and does not override existing variables", () => {
    expect(parseDotenv('export OPENCODE_API_KEY="abc\\n"\n# comment\nEMPTY=\nPLAIN=value # note\n')).toEqual({
      OPENCODE_API_KEY: "abc\n",
      EMPTY: "",
      PLAIN: "value",
    });
    expect(() => parseDotenv("TOKEN=super-secret-value\nNOT KEY=x\n")).toThrow("line 2");
    const env: NodeJS.ProcessEnv = { OPENCODE_API_KEY: "from-shell" };
    const loaded = loadDotenv(
      writeTempEnv("OPENCODE_API_KEY=from-file\nTELEGRAM_BOT_TOKEN=token-value\n"),
      env,
    );
    expect(loaded).toEqual(["TELEGRAM_BOT_TOKEN"]);
    expect(env.OPENCODE_API_KEY).toBe("from-shell");
    expect(env.TELEGRAM_BOT_TOKEN).toBe("token-value");
  });

  test("environment provider keys override discovered service credentials", async () => {
    const env = { OPENCODE_API_KEY: "env-key", FORGE_CUSTOM_API_KEY: "custom-key" };
    expect(credentialsFromEnv(new Set(["opencode", "custom"]), env)).toEqual({
      opencode: { settings: { apiKey: "env-key" } },
      custom: { settings: { apiKey: "custom-key" } },
    });
    const merged = await providerCredentials(new Set(["opencode"]), {
      env,
      discover: async () => ({ opencode: { settings: { apiKey: "service-key" } } }),
    });
    expect(merged.opencode?.settings.apiKey).toBe("env-key");
    await expect(
      providerCredentials(new Set(["missing"]), {
        env: {},
        discover: async () => {
          throw new Error("service down");
        },
      }),
    ).rejects.toThrow("service down");
  });
});

describe("Telegram access and delivery", () => {
  test("rejects another user without treating the chat as authorized", () => {
    const foreign = authorize(update(7, 99, 50, "/status"), { ownerId: 42, chatId: null });
    expect(foreign).toEqual({ ok: false, reason: "unauthorized", chatId: 50 });
    const group = authorize(
      {
        update_id: 1,
        message: { message_id: 1, text: "/status", chat: { id: -1, type: "group" }, from: { id: 42 } },
      },
      { ownerId: 42, chatId: null },
    );
    expect(group).toEqual({ ok: false, reason: "ignored" });
    const owner = authorize(update(3, 42, 50, "/status"), { ownerId: 42, chatId: 50 });
    expect(owner.ok).toBe(true);
  });

  test("splits replies and redacts the bot token from transport errors", async () => {
    expect(splitText(`one\n${"x".repeat(20)}`, 10).every((part) => part.length <= 10)).toBe(true);
    const client = new TelegramClient(
      "123:secret-token",
      Object.assign(
        async () => {
          throw new Error("failed at https://api.telegram.org/bot123:secret-token/getUpdates");
        },
        { preconnect: () => undefined },
      ),
    );
    await expect(client.getUpdates(0, 0)).rejects.toThrow("<redacted>");
    expect(redactSecrets("token is 123:secret-token", ["123:secret-token"])).toBe("token is <redacted>");
  });

  test("a replayed owner update does not unpark twice, and a stranger sees no factory data", async () => {
    const root = await fixtureRepo();
    try {
      writeJSON(join(root, ".work", ".harness", "parked.json"), { "T-001": "needs a decision" });
      const sent: string[] = [];
      const transport = fakeTransport([update(4, 42, 7, "/unpark T-001"), update(5, 99, 8, "/status")], sent);
      const service = serviceFor(root, transport);
      await drainOnce(service);
      await drainOnce(service);
      expect(sent.filter((text) => text.startsWith("Unparked"))).toHaveLength(1);
      expect(sent.some((text) => text === "Unauthorized.")).toBe(true);
      expect(sent.some((text) => text.includes("Ready:"))).toBe(false);
      expect(loadOperatorState(root).processed).toEqual(["4", "5"]);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("expired callback query ack and non-retryable 4xx do not wedge the outbox", async () => {
    const methods: string[] = [];
    const client = new TelegramClient(
      "123:secret-token",
      Object.assign(
        async (input: RequestInfo | URL) => {
          const url = String(input);
          if (url.endsWith("/answerCallbackQuery")) {
            methods.push("answerCallbackQuery");
            return new Response(
              JSON.stringify({ ok: false, error_code: 400, description: "query is too old" }),
              { status: 400 },
            );
          }
          methods.push("sendMessage");
          return new Response(JSON.stringify({ ok: true, result: { message_id: 9 } }), { status: 200 });
        },
        { preconnect: () => undefined },
      ),
    );
    const res = await client.send({ chatId: 7, text: "published", callbackId: "cb-expired" });
    expect(res.messageId).toBe(9);
    expect(methods).toEqual(["answerCallbackQuery", "sendMessage"]);

    const root = await fixtureRepo();
    try {
      const sent: string[] = [];
      writeJSON(join(root, ".work", ".harness", "operator", "state.json"), {
        outbox: [
          { chatId: 7, text: "bad-400" },
          { chatId: 7, text: "good-after-400" },
        ],
      });
      const transport: TelegramTransport = {
        async getUpdates() {
          return [];
        },
        async send(req) {
          if (req.text === "bad-400") throw new TelegramError("Bad Request", false, 400);
          sent.push(req.text);
          return { messageId: 1 };
        },
      };
      await drainOnce(serviceFor(root, transport));
      expect(sent).toEqual(["good-after-400"]);
      expect(loadOperatorState(root).outbox).toEqual([]);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("/cancel and /halt are processed while a conversation turn is still running", async () => {
    const root = await fixtureRepo();
    try {
      const sent: string[] = [];
      let turnStarted = false;
      let cancelServed = false;
      let abortTurn: (() => void) | null = null;
      const runtime: ConversationalRuntime = {
        async reply() {
          turnStarted = true;
          await new Promise<void>((_, reject) => {
            abortTurn = () => reject(new Error("Conversation turn cancelled"));
          });
          return { text: "never", sessionID: "ses_1", confirms: [] };
        },
        async cancel() {
          abortTurn?.();
        },
        async close() {},
      };
      const transport: TelegramTransport = {
        async getUpdates(offset) {
          if (offset === 0) return [update(10, 42, 7, "Please inspect the queue")];
          if (turnStarted && !cancelServed) {
            cancelServed = true;
            return [update(11, 42, 7, "/halt"), update(12, 42, 7, "/cancel")];
          }
          return [];
        },
        async send(request) {
          sent.push(request.text);
          return { messageId: sent.length };
        },
      };
      await drainOnce({ ...serviceFor(root, transport), runtime });
      expect(queueLiveness(root).halted).toBe(true);
      expect(sent.some((text) => text.includes("Queue halted"))).toBe(true);
      expect(sent.filter((text) => text.includes("Cancelled the current conversation turn."))).toHaveLength(
        1,
      );
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});

describe("queue control and owner changes", () => {
  test("halt and resume use the launcher without starting a second queue", async () => {
    const root = mkdtempSync(join(tmpdir(), "openforge-queue-"));
    const spawned: string[][] = [];
    const launcher: ProcessLauncher = {
      spawn(request) {
        spawned.push(request.command);
        return { pid: process.pid };
      },
    };
    try {
      const config = parseConfig({});
      expect((await resumeQueue(root, config, 2, launcher)).includes(`pid ${process.pid}`)).toBe(true);
      expect(queueLiveness(root).running).toBe(true);
      expect(await haltQueue(root)).toContain("Halt requested");
      expect(queueLiveness(root).halted).toBe(true);
      await expect(resumeQueue(root, config, undefined, launcher)).resolves.toContain("still running");
      expect(spawned).toHaveLength(1);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("operation replay returns the recorded result", async () => {
    const root = mkdtempSync(join(tmpdir(), "openforge-journal-"));
    try {
      let calls = 0;
      const first = await once(root, "op-1", async () => {
        calls++;
        return "done";
      });
      const second = await once(root, "op-1", async () => {
        calls++;
        return "done";
      });
      expect(first.repeated).toBe(false);
      expect(second.repeated).toBe(true);
      expect(calls).toBe(1);
      expect(await unparkOnce(root, "T-001", "op-1")).toContain("already applied");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("prepares a task off the base branch and publishes only on confirmation", async () => {
    const root = await fixtureRepo();
    try {
      const config = parseConfig({ operator: { decisions: "docs/decisions.md" } });
      const prepared = await prepareTaskChange(root, config, {
        operation_id: "tg4task",
        title: "Add a focused check",
        goal: "Cover the missing failure path.",
        contract: "Add the check without changing unrelated packages.",
        owns: ["src/"],
        check: "true",
        acceptance: ["unit_example"],
        resources: [],
        depends_on: [],
        milestone: "M1",
        block_task: "T-001",
      });
      expect(prepared.status).toBe("prepared");
      expect(readFileSync(join(root, "docs/tasks/T-001-demo.md"), "utf8")).not.toContain("T-002");
      const published = await publishChange(root, config, "tg4task");
      expect(published).toContain("Published");
      const tasks = loadTasks(root, config.tasks);
      expect(tasks.has("T-002")).toBe(true);
      expect(tasks.get("T-001")?.dependsOn).toContain("T-002");
      expect(await publishChange(root, config, "tg4task")).toContain("Already published");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("publishes a prepared task cleanly after another task lands and updates README.md on main", async () => {
    const root = await fixtureRepo();
    try {
      const config = parseConfig({
        operator: {
          decisions: "docs/decisions.md",
          docsCheck: 'grep -q "T-002" docs/tasks/README.md',
        },
      });
      writeFileSync(
        join(root, "docs/tasks/T-000-done.md"),
        `---
id: T-000
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [docs/]
check: true
acceptance: []
---
# T-000 — Prerequisite task
`,
      );
      writeIndex(loadTasks(root), join(root, config.index), false, config);
      await git(root, "add", "-A");
      await git(root, "commit", "-m", "add T-000");

      await prepareTaskChange(root, config, {
        operation_id: "tg-rebase-index",
        title: "Follow-up task",
        goal: "Verify rebase with index regeneration.",
        contract: "Stay in owned path.",
        owns: ["other/"],
        check: "true",
        acceptance: [],
        resources: [],
        depends_on: [],
        milestone: "M1",
      });

      const doneContent = readFileSync(join(root, "docs/tasks/T-000-done.md"), "utf8").replace(
        "status: planned",
        "status: done",
      );
      writeFileSync(join(root, "docs/tasks/T-000-done.md"), doneContent);
      writeIndex(loadTasks(root), join(root, config.index), false, config);
      await git(root, "add", "-A");
      await git(root, "commit", "-m", "land T-000 on main");

      const published = await publishChange(root, config, "tg-rebase-index");
      expect(published).toContain("Published");
      const readme = readFileSync(join(root, "docs/tasks/README.md"), "utf8");
      expect(readme).toContain("[T-000](T-000-done.md)");
      expect(readme).toContain("[T-002](T-002-follow-up-task.md)");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("operatorPermissions denies read, grep, and glob on .env and .work", () => {
    const perms = operatorPermissions();
    for (const action of ["read", "grep", "glob"]) {
      expect(perms).toContainEqual({ action, resource: ".env", effect: "deny" });
      expect(perms).toContainEqual({ action, resource: "**/.env*", effect: "deny" });
      expect(perms).toContainEqual({ action, resource: ".work/**", effect: "deny" });
      expect(perms).toContainEqual({ action, resource: "**/.harness/**", effect: "deny" });
    }
  });

  test("inserts and replaces decision rows", () => {
    const markdown =
      "| ID | Status / decision | Needed by |\n| --- | --- | --- |\n| O-01 | Resolved | T-001 |\n";
    expect(nextDecisionId(markdown)).toBe("O-02");
    const inserted = upsertDecisionRow(markdown, { id: "O-02", status: "Open: choose A", neededBy: "T-001" });
    expect(inserted).toContain("| O-02 | Open: choose A | T-001 |");
    const replaced = upsertDecisionRow(inserted, { id: "O-02", status: "Resolved: A", neededBy: "T-001" });
    expect(replaced).toContain("| O-02 | Resolved: A | T-001 |");
    expect(replaced.match(/O-02/g)).toHaveLength(1);
    expect(nextTaskId(new Map([["T-009", {} as never]]))).toBe("T-010");
  });
});

function writeTempEnv(content: string): string {
  const dir = mkdtempSync(join(tmpdir(), "openforge-dotenv-"));
  const path = join(dir, ".env");
  writeFileSync(path, content);
  return path;
}

function update(id: number, userId: number, chatId: number, text: string): TelegramUpdate {
  return {
    update_id: id,
    message: { message_id: id, text, chat: { id: chatId, type: "private" }, from: { id: userId } },
  };
}

function fakeTransport(updates: TelegramUpdate[], sent: string[]): TelegramTransport {
  return {
    async getUpdates(offset) {
      return updates.filter((item) => item.update_id >= offset);
    },
    async send(request) {
      sent.push(request.text);
      return { messageId: sent.length };
    },
  };
}

function serviceFor(primary: string, transport: TelegramTransport): OperatorService {
  return {
    primary,
    config: parseConfig({}),
    ownerId: 42,
    chatId: null,
    transport,
    runtime: null,
  };
}

async function fixtureRepo(): Promise<string> {
  const root = mkdtempSync(join(tmpdir(), "openforge-operator-"));
  await git(root, "init", "-b", "main");
  await git(root, "config", "user.name", "Tester");
  await git(root, "config", "user.email", "tester@example.com");
  mkdirSync(join(root, "docs/tasks"), { recursive: true });
  mkdirSync(join(root, "src"), { recursive: true });
  writeFileSync(join(root, "src/demo.txt"), "demo\n");
  writeFileSync(
    join(root, "docs/tasks/T-001-demo.md"),
    `---
id: T-001
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [src/]
check: true
acceptance: [demo]
---
# T-001 — Demo task
`,
  );
  writeFileSync(
    join(root, "docs/decisions.md"),
    "| ID | Status / decision | Needed by |\n| --- | --- | --- |\n| O-01 | Resolved: example | T-001 |\n",
  );
  const readme = join(root, "docs/tasks/README.md");
  writeFileSync(
    readme,
    `# Tasks\n\n${READY_START}\n${READY_END}\n\n## Dependency index\n\n| Task | Dependencies | Status / decision |\n| --- | --- | --- |\n`,
  );
  const config = parseConfig({});
  writeIndex(loadTasks(root), readme, false, config);
  writeFileSync(join(root, "openforge.json"), `${JSON.stringify(config, null, 2)}\n`);
  await git(root, "add", "-A");
  await git(root, "commit", "-m", "init");
  return root;
}
