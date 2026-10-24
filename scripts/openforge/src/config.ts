import { readFileSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";

export const ROLES = ["planner", "implementer", "fixer", "reviewer"] as const;
export type Role = (typeof ROLES)[number];

export const CONFIG_FILE = "openforge.json";
export const DEFAULT_SDK_VERSION = "2.1.2";

export const DEFAULT_ROLE_MODELS: Record<Role, string> = {
  planner: "opencode/gpt-6-astra",
  implementer: "opencode/claude-sonnet-5-5",
  fixer: "opencode/claude-sonnet-5-5",
  reviewer: "opencode/gpt-6-astra",
};

const positiveInt = z.number().int().positive();
const nonNegInt = z.number().int().nonnegative();
const positiveSec = z.number().positive();
const modelRef = z.string().regex(/^[^/\s]+\/\S+$/, "model must be provider/model");

const roleEntry = (role: Role) =>
  z.strictObject({ model: modelRef }).default({ model: DEFAULT_ROLE_MODELS[role] });

const operatorSchema = z.strictObject({
  model: modelRef.default(DEFAULT_ROLE_MODELS.planner),
  tokenEnv: z.string().min(1).default("TELEGRAM_BOT_TOKEN"),
  ownerIdEnv: z.string().min(1).default("TELEGRAM_OWNER_ID"),
  chatIdEnv: z.string().min(1).default("TELEGRAM_CHAT_ID"),
  decisions: z.string().min(1).optional(),
  docsCheck: z.string().min(1).optional(),
  turnTimeoutSeconds: positiveSec.default(900),
  pollTimeoutSeconds: z.number().int().min(0).max(50).default(25),
});

function defaultOperator() {
  return {
    model: DEFAULT_ROLE_MODELS.planner,
    tokenEnv: "TELEGRAM_BOT_TOKEN",
    ownerIdEnv: "TELEGRAM_OWNER_ID",
    chatIdEnv: "TELEGRAM_CHAT_ID",
    turnTimeoutSeconds: 900,
    pollTimeoutSeconds: 25,
  };
}

export const configSchema = z.strictObject({
  tasks: z.string().min(1).default("docs/tasks/T-*.md"),
  index: z.string().min(1).default("docs/tasks/README.md"),
  base: z.string().min(1).default("main"),
  setup: z.string().min(1).optional(),
  check: z.string().min(1).default("make test"),
  mainCheck: z
    .union([z.string().min(1), z.array(z.string().min(1)).min(1)])
    .default(["make lint", "make test"]),
  mainCheckEvery: nonNegInt.default(5),
  mainCheckOnDrain: z.boolean().default(true),
  env: z.record(z.string(), z.string()).default({}),
  envPassthrough: z.array(z.string().min(1)).default([]),
  resources: z.record(z.string(), positiveInt).default({}),
  protected: z.array(z.string().min(1)).default([]),
  context: z.array(z.string().min(1)).default(["AGENTS.md"]),
  roles: z
    .strictObject({
      planner: roleEntry("planner"),
      implementer: roleEntry("implementer"),
      fixer: roleEntry("fixer"),
      reviewer: roleEntry("reviewer"),
    })
    .default(() => ({
      planner: { model: DEFAULT_ROLE_MODELS.planner },
      implementer: { model: DEFAULT_ROLE_MODELS.implementer },
      fixer: { model: DEFAULT_ROLE_MODELS.fixer },
      reviewer: { model: DEFAULT_ROLE_MODELS.reviewer },
    })),
  limits: z
    .strictObject({
      workers: positiveInt.default(5),
      repairs: nonNegInt.default(3),
      reviewRounds: positiveInt.default(2),
      agentTimeoutSeconds: positiveSec.default(10800),
      checkTimeoutSeconds: positiveSec.default(1800),
      agentAttempts: positiveInt.default(3),
    })
    .default(() => ({
      workers: 5,
      repairs: 3,
      reviewRounds: 2,
      agentTimeoutSeconds: 10800,
      checkTimeoutSeconds: 1800,
      agentAttempts: 3,
    })),
  sdkVersion: z.string().min(1).default(DEFAULT_SDK_VERSION),
  operator: operatorSchema.default(defaultOperator),
});

export type Config = z.infer<typeof configSchema>;

export function configPath(root: string): string {
  return join(root, CONFIG_FILE);
}

export function parseConfig(value: unknown, source = CONFIG_FILE): Config {
  const parsed = configSchema.safeParse(value);
  if (!parsed.success) {
    const issues = parsed.error.issues.map((i) => `${i.path.join(".") || "<root>"}: ${i.message}`);
    throw new Error(`${source}: invalid config: ${issues.join("; ")}`);
  }
  return parsed.data;
}

export function loadConfig(root: string): Config {
  const path = configPath(root);
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(path, "utf8"));
  } catch (error) {
    throw new Error(`${path}: cannot read config: ${error instanceof Error ? error.message : String(error)}`);
  }
  return parseConfig(raw, path);
}

export function resourcePrefix(taskId: string): string {
  return taskId.toLowerCase().replace(/[^a-z0-9]/g, "");
}

export function expandTemplate(template: string, task: { id: string; slug: string }): string {
  const prefix = resourcePrefix(task.id);
  return template.replaceAll("{id}", task.id).replaceAll("{slug}", task.slug).replaceAll("{prefix}", prefix);
}

export function buildTaskEnv(
  config: Config,
  home: string,
  task?: { id: string; slug: string },
): Record<string, string> {
  const env: Record<string, string> = {
    FORGE_HALT_FILE: join(home, "HALT"),
  };
  if (task) {
    env.FORGE_TASK_ID = task.id;
    env.FORGE_RESOURCE_PREFIX = resourcePrefix(task.id);
    for (const [key, tmpl] of Object.entries(config.env)) {
      env[key] = expandTemplate(tmpl, task);
    }
  }
  return env;
}

const INHERITED_ENV = [
  "PATH",
  "HOME",
  "USER",
  "LOGNAME",
  "SHELL",
  "LANG",
  "LANGUAGE",
  "TZ",
  "TMPDIR",
  "LC_*",
  "XDG_*",
  "GO*",
  "CGO_*",
];

function matchesEnvName(name: string, pattern: string): boolean {
  return pattern.endsWith("*") ? name.startsWith(pattern.slice(0, -1)) : name === pattern;
}

/**
 * Agent session shells run with exactly the variables the session environment
 * holds (the SDK replaces, rather than extends, the process environment), so
 * the toolchain variables must be passed explicitly. Only a safe allowlist plus
 * `envPassthrough` is inherited; credentials stay out of agent shells.
 */
export function inheritedEnv(
  config: Pick<Config, "envPassthrough">,
  source: NodeJS.ProcessEnv = process.env,
): Record<string, string> {
  const patterns = [...INHERITED_ENV, ...config.envPassthrough];
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(source)) {
    if (value !== undefined && patterns.some((pattern) => matchesEnvName(name, pattern))) {
      env[name] = value;
    }
  }
  return env;
}
