import { OpenCode } from "@opencode/client";
import { Service } from "@opencode/client/service";

export type ProviderConfig = Record<string, { settings: { apiKey: string } }>;

export interface DiscoveredCredential {
  active: boolean;
  integrationID: string;
  value: { type: string; key?: string };
}

/** Environment variables that supply a provider key when no service credential is active. */
export const PROVIDER_ENV_KEYS: Record<string, string> = {
  opencode: "OPENCODE_API_KEY",
  openai: "OPENAI_API_KEY",
  anthropic: "ANTHROPIC_API_KEY",
  google: "GOOGLE_API_KEY",
  gemini: "GEMINI_API_KEY",
};

export function credentialsFromEnv(
  providers: ReadonlySet<string>,
  env: NodeJS.ProcessEnv = process.env,
): ProviderConfig {
  const configuration: ProviderConfig = {};
  for (const provider of providers) {
    const configured = PROVIDER_ENV_KEYS[provider];
    const generic = `FORGE_${provider.toUpperCase().replace(/[^A-Z0-9]/g, "_")}_API_KEY`;
    const value = (configured ? env[configured] : undefined) || env[generic];
    if (value) configuration[provider] = { settings: { apiKey: value } };
  }
  return configuration;
}

async function discoverServiceCredentials(providers: ReadonlySet<string>): Promise<ProviderConfig> {
  const configuration: ProviderConfig = {};
  const endpoint = await Service.discover();
  if (!endpoint) return configuration;
  const client = OpenCode.make({ baseUrl: endpoint.url, headers: Service.headers(endpoint) });
  const credentials = (await client.credential.list({
    signal: AbortSignal.timeout(10_000),
  })) as DiscoveredCredential[];
  for (const credential of credentials) {
    if (!credential.active || !providers.has(credential.integrationID)) continue;
    if (credential.value.type === "key" && credential.value.key) {
      configuration[credential.integrationID] = { settings: { apiKey: credential.value.key } };
    }
  }
  return configuration;
}

/**
 * Service credentials fill providers that have no environment key. An explicit
 * environment key wins so a host `.env` is the operator-selected credential.
 * Discovery failure is ignored when every requested provider already has an
 * environment key; otherwise the original error is reported.
 */
export async function providerCredentials(
  providers: ReadonlySet<string>,
  options: {
    env?: NodeJS.ProcessEnv;
    discover?: (providers: ReadonlySet<string>) => Promise<ProviderConfig>;
  } = {},
): Promise<ProviderConfig> {
  const env = options.env ?? process.env;
  const fromEnv = credentialsFromEnv(providers, env);
  let service: ProviderConfig = {};
  let serviceError: unknown;
  try {
    service = await (options.discover ?? discoverServiceCredentials)(providers);
  } catch (error) {
    serviceError = error;
  }
  const merged: ProviderConfig = { ...service, ...fromEnv };
  const missing = [...providers].filter((id) => !merged[id]);
  if (serviceError && missing.length > 0) throw serviceError;
  return merged;
}
