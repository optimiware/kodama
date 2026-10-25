import { Effect } from "effect";
import { Auth } from "../../route/auth.js";
import { AIError } from "../../schema/index.js";
/**
 * AWS credentials for SigV4 signing. Bedrock also supports Bearer API key auth,
 * which provider facades configure as route auth instead of SigV4.
 */
export interface Credentials {
    readonly region: string;
    readonly accessKeyId: string;
    readonly secretAccessKey: string;
    readonly sessionToken?: string;
}
/** Static credentials or an effect resolved before every request. */
export type CredentialSource = Credentials | Effect.Effect<Credentials, AIError>;
export interface DefaultChainOptions {
    readonly region: string;
    /** Shared config profile passed to the AWS default chain. */
    readonly profile?: string;
}
/**
 * Resolve credentials through the AWS default provider chain: environment
 * variables, shared config and SSO caches, web identity tokens, process
 * credentials, and container or instance metadata. A fresh chain runs on every
 * request so credentials rotated on disk without an expiration (for example
 * shared-config keys rewritten by a corporate SSO tool) are always re-read;
 * the SDK's own memoization would otherwise pin them for the process lifetime.
 */
export declare const defaultChain: (options: DefaultChainOptions) => Effect.Effect<Credentials, AIError>;
/** Sign the exact JSON bytes with SigV4 using credentials configured on the route. */
export declare const sigV4: (source: CredentialSource | undefined, options?: {
    readonly service?: string;
    readonly name?: string;
}) => Auth.Definition;
/** Bedrock route auth defaults to SigV4 and expects credentials from route configuration. */
export declare const auth: Auth.Definition;
export declare const resolveRegion: (input: {
    readonly region?: string;
    readonly credentials?: {
        readonly region: string;
    };
}) => string;
export interface ResolveAuthInput {
    readonly apiKey?: string;
    readonly credentials?: Credentials;
    readonly profile?: string;
}
export interface ResolveAuthOptions {
    readonly service?: string;
    readonly name?: string;
    /** `sigv4` ignores an ambient `AWS_BEARER_TOKEN_BEDROCK`; `bearer` is validated by the caller. */
    readonly mode?: "bearer" | "sigv4";
}
/**
 * Bearer wins over SigV4 and explicit static credentials win over the default
 * chain, matching the AWS SDK's own precedence for `AWS_BEARER_TOKEN_BEDROCK`.
 * The region is applied to the SigV4 scope so it always matches the endpoint host.
 */
export declare const resolveAuth: (input: ResolveAuthInput, region: string, options?: ResolveAuthOptions) => Auth.Definition;
export * as BedrockAuth from "./bedrock-auth.js";
