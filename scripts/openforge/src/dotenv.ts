import { existsSync, readFileSync } from "node:fs";

const KEY = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function parseDotenv(text: string): Record<string, string> {
  const values: Record<string, string> = {};
  const lines = text
    .replace(/^\uFEFF/, "")
    .replace(/\r\n/g, "\n")
    .replace(/\r/g, "\n")
    .split("\n");
  for (let index = 0; index < lines.length; index++) {
    const rawLine = lines[index] ?? "";
    const trimmed = rawLine.trim();
    if (trimmed === "" || trimmed.startsWith("#")) continue;
    const line = trimmed.startsWith("export ") ? trimmed.slice(7).trim() : trimmed;
    const eq = line.indexOf("=");
    if (eq <= 0) throw new Error(`dotenv: line ${index + 1}: invalid assignment`);
    const key = line.slice(0, eq).trim();
    if (!KEY.test(key)) throw new Error(`dotenv: line ${index + 1}: invalid name`);
    values[key] = parseValue(line.slice(eq + 1), index + 1);
  }
  return values;
}

function parseValue(raw: string, line: number): string {
  if (raw.startsWith("'")) {
    const end = raw.indexOf("'", 1);
    if (end < 0) throw new Error(`dotenv: line ${line}: unterminated quote`);
    return raw.slice(1, end);
  }
  if (raw.startsWith('"')) {
    let value = "";
    for (let i = 1; i < raw.length; i++) {
      const ch = raw[i];
      if (ch === "\\") {
        const next = raw[i + 1];
        if (next === undefined) throw new Error(`dotenv: line ${line}: dangling escape`);
        value += next === "n" ? "\n" : next === "r" ? "\r" : next === "t" ? "\t" : next;
        i++;
        continue;
      }
      if (ch === '"') return value;
      value += ch;
    }
    throw new Error(`dotenv: line ${line}: unterminated quote`);
  }
  const comment = raw.search(/\s+#/);
  return (comment >= 0 ? raw.slice(0, comment) : raw).trim();
}

const dotenvValues = new Set<string>();

/** Load KEY=VALUE pairs. Existing non-empty variables win. Returns names that were set. */
export function loadDotenv(
  path: string,
  target: NodeJS.ProcessEnv = process.env,
  options: { override?: boolean } = {},
): string[] {
  if (!existsSync(path)) return [];
  const parsed = parseDotenv(readFileSync(path, "utf8"));
  const loaded: string[] = [];
  for (const [key, value] of Object.entries(parsed)) {
    if (value.length >= 8) dotenvValues.add(value);
    const current = target[key];
    if (!options.override && current !== undefined && current !== "") {
      if (current.length >= 8) dotenvValues.add(current);
      continue;
    }
    target[key] = value;
    loaded.push(key);
  }
  return loaded;
}

const SECRET_NAME = /TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL/i;

export function secretValues(env: NodeJS.ProcessEnv = process.env): string[] {
  const values = new Set<string>(dotenvValues);
  for (const [name, value] of Object.entries(env)) {
    if (!value || value.length < 8 || !SECRET_NAME.test(name)) continue;
    values.add(value);
  }
  return [...values];
}

export function redactSecrets(text: string, secrets: readonly string[]): string {
  let out = text;
  for (const secret of secrets) {
    if (secret.length < 8) continue;
    out = out.split(secret).join("<redacted>");
  }
  return out;
}
