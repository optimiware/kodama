import { Plugin } from "@opencode/plugin";
import { z } from "zod";
import { prepareDecisionInput, prepareTaskInput } from "../control/changes.ts";
import { redactSecrets, secretValues } from "../dotenv.ts";

export interface TurnContext {
  operationPrefix: string;
  confirms: Array<{ operationId: string; label: string; summary: string }>;
  notes: string[];
}

export interface OperatorControls {
  status(): string;
  task(id: string): string;
  events(limit: number): string;
  halt(): Promise<string>;
  resume(workers?: number): Promise<string>;
  unpark(taskId: string, operationId: string): Promise<string>;
  retryMain(operationId: string): Promise<string>;
  prepareTask(input: z.infer<typeof prepareTaskInput>): Promise<string>;
  prepareDecision(input: z.infer<typeof prepareDecisionInput>): Promise<string>;
}

const taskId = z.object({ id: z.string().regex(/^T-\d+$/) });
const unparkInput = z.object({
  id: z.string().regex(/^T-\d+$/),
  operation_id: z.string().min(1),
});
const workers = z.object({ workers: z.number().int().min(1).max(64).optional() });
const eventsInput = z.object({ limit: z.number().int().min(1).max(30).optional() });

function text(value: string): { content: string } {
  return { content: redactSecrets(value, secretValues()) };
}

function requireOperation(id: string, turn: TurnContext): void {
  if (!turn.operationPrefix || !id.startsWith(turn.operationPrefix)) {
    throw new Error(`operation_id must start with ${turn.operationPrefix || "the current turn id"}`);
  }
}

const allowedCoreTools = new Set(["read", "glob", "grep"]);

export function operatorPlugin(controls: OperatorControls, turn: TurnContext) {
  return Plugin.define({
    id: "openforge-operator",
    async setup(ctx) {
      await ctx.mcp.transform((editor) => {
        for (const [name] of editor.list()) editor.remove(name);
      });
      await ctx.tool.transform((editor) => {
        for (const tool of editor.list()) {
          if (!allowedCoreTools.has(tool.id)) editor.remove(tool.id);
        }
        editor.namespace({
          name: "factory",
          description: "Inspect and operate the OpenForge queue. Publication requires owner confirmation.",
        });
        const add = <T>(
          name: string,
          description: string,
          schema: z.ZodType<T>,
          execute: (input: T) => Promise<string>,
        ) => {
          editor.add({
            name,
            description,
            input: z.toJSONSchema(schema),
            options: { namespace: "factory", codemode: false },
            async execute(input: unknown) {
              const result = await execute(schema.parse(input));
              turn.notes.push(result);
              return text(result);
            },
          });
        };
        add("status", "Show queue, ready, active, and parked tasks.", z.object({}), async () =>
          controls.status(),
        );
        add("task", "Show one task, including its park reason and check-log tail.", taskId, async (input) =>
          controls.task(input.id),
        );
        add("events", "Show recent factory events.", eventsInput, async (input) =>
          controls.events(input.limit ?? 15),
        );
        add("halt", "Request a graceful queue halt.", z.object({}), async () => controls.halt());
        add("resume", "Clear a halt and start the queue if it is stopped.", workers, async (input) =>
          controls.resume(input.workers),
        );
        add("unpark", "Requeue a parked task. Use the turn operation id.", unparkInput, async (input) => {
          requireOperation(input.operation_id, turn);
          return controls.unpark(input.id, input.operation_id);
        });
        add(
          "retry_main",
          "Start the periodic main check again. Use the turn operation id.",
          z.object({ operation_id: z.string().min(1) }),
          async (input) => {
            requireOperation(input.operation_id, turn);
            return controls.retryMain(input.operation_id);
          },
        );
        editor.add({
          name: "prepare_task",
          description: "Create a validated task change. It is not published until the owner confirms.",
          input: z.toJSONSchema(prepareTaskInput),
          options: { namespace: "factory", codemode: false },
          async execute(input: unknown) {
            const parsed = prepareTaskInput.parse(input);
            requireOperation(parsed.operation_id, turn);
            const summary = await controls.prepareTask(parsed);
            turn.notes.push(summary);
            turn.confirms.push({
              operationId: parsed.operation_id,
              label: `Publish ${parsed.operation_id}`,
              summary,
            });
            return text(summary);
          },
        });
        editor.add({
          name: "prepare_decision",
          description: "Record an owner decision. It is not published until the owner confirms.",
          input: z.toJSONSchema(prepareDecisionInput),
          options: { namespace: "factory", codemode: false },
          async execute(input: unknown) {
            const parsed = prepareDecisionInput.parse(input);
            requireOperation(parsed.operation_id, turn);
            const summary = await controls.prepareDecision(parsed);
            turn.notes.push(summary);
            turn.confirms.push({
              operationId: parsed.operation_id,
              label: `Publish ${parsed.operation_id}`,
              summary,
            });
            return text(summary);
          },
        });
      });
      await ctx.tool.hook("execute.before", (event) => {
        const blob = JSON.stringify(event.input ?? {});
        if (/(^|[^A-Za-z0-9_])\.env([^A-Za-z0-9_]|$)/.test(blob) || blob.includes("opencode.db")) {
          throw new Error("Refusing to read credential or runtime database files");
        }
      });
      await ctx.tool.hook("execute.after", (event) => {
        if (event.status !== "completed") return;
        const secrets = secretValues();
        const result = event.result as { content?: string | Array<{ type?: string; text?: string }> };
        if (typeof result.content === "string") {
          (result as { content: string }).content = redactSecrets(result.content, secrets);
        } else if (Array.isArray(result.content)) {
          for (const part of result.content) {
            if (part.type === "text" && part.text) part.text = redactSecrets(part.text, secrets);
          }
        }
      });
    },
  });
}

export function operatorPermissions() {
  const allow = [
    "read",
    "glob",
    "grep",
    "factory_status",
    "factory_task",
    "factory_events",
    "factory_halt",
    "factory_resume",
    "factory_unpark",
    "factory_retry_main",
    "factory_prepare_task",
    "factory_prepare_decision",
  ];
  const restrictedResources = [
    ".env",
    ".env*",
    "**/.env",
    "**/.env*",
    ".work",
    ".work/**",
    ".harness",
    ".harness/**",
    "**/.harness/**",
  ];
  const restrictedActions = ["read", "grep", "glob"];
  return [
    { action: "*", resource: "*", effect: "deny" as const },
    ...allow.map((action) => ({ action, resource: "*", effect: "allow" as const })),
    ...restrictedActions.flatMap((action) =>
      restrictedResources.map((resource) => ({ action, resource, effect: "deny" as const })),
    ),
    { action: "shell", resource: "*", effect: "deny" as const },
    { action: "edit", resource: "*", effect: "deny" as const },
    { action: "write", resource: "*", effect: "deny" as const },
  ];
}
