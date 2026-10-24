import { Plugin } from "@opencode/plugin";
import { z } from "zod";
import type { Role } from "../config.ts";
import {
  type HandoffResult,
  handoffSchema,
  type PlanResult,
  planSchema,
  type ReviewResult,
  reviewSchema,
} from "../gate.ts";
import { REPORT_TOOLS } from "../prompts.ts";
import { canonicalJSON } from "../system.ts";

export type RoleResult = PlanResult | HandoffResult | ReviewResult;

export interface ActiveInvocation {
  role: Role;
  worktree: string;
  result?: RoleResult;
}

const coreTools = new Set(["read", "glob", "grep", "shell", "patch", "edit", "write"]);

export function reportingPlugin(active: Map<string, ActiveInvocation>) {
  return Plugin.define({
    id: "openforge",
    async setup(ctx) {
      await ctx.mcp.transform((editor) => {
        for (const [name] of editor.list()) editor.remove(name);
      });
      await ctx.tool.transform((editor) => {
        for (const tool of editor.list()) {
          if (!coreTools.has(tool.id)) editor.remove(tool.id);
        }
        editor.namespace({
          name: "openforge",
          description: "Submit the structured stage result to OpenForge.",
        });
        for (const [name, schema, roles] of [
          ["submit_plan", planSchema, ["planner"]],
          ["submit_handoff", handoffSchema, ["implementer", "fixer"]],
          ["submit_review", reviewSchema, ["reviewer"]],
        ] as const) {
          editor.add({
            name,
            description: "Submit your final stage result once.",
            input: z.toJSONSchema(schema),
            options: { namespace: "openforge", codemode: false },
            async execute(input: unknown, context) {
              const invocation = active.get(context.sessionID);
              if (
                !invocation ||
                !(roles as readonly string[]).includes(invocation.role) ||
                context.agent !== invocation.role
              ) {
                throw new Error("Result does not belong to this stage");
              }
              await ctx.permission.assert({
                sessionID: context.sessionID,
                agent: context.agent,
                action: `openforge_${name}`,
                resources: ["*"],
              });
              const parsed = schema.parse(input);
              if (
                invocation.result !== undefined &&
                canonicalJSON(invocation.result) !== canonicalJSON(parsed)
              ) {
                throw new Error("This invocation already submitted a different result");
              }
              invocation.result = parsed;
              return {
                content: "Result recorded. End this stage now.",
              };
            },
          });
        }
      });
      await ctx.tool.hook("execute.before", (event) => {
        const invocation = active.get(event.sessionID);
        if (!invocation) throw new Error("OpenForge has no active invocation for this session");
        if (event.agent !== invocation.role) throw new Error("Agent role changed during execution");
        if (invocation.result !== undefined && event.tool !== REPORT_TOOLS[invocation.role]) {
          throw new Error("The stage result has already been submitted");
        }
        if (
          event.tool === "shell" &&
          typeof event.input === "object" &&
          event.input !== null &&
          "background" in event.input &&
          event.input.background === true
        ) {
          throw new Error("Commands must run in the foreground");
        }
      });
    },
  });
}
