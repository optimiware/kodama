import { Config, Effect, Redacted } from "effect";
import { Headers } from "effect/http";
import { AIError, type HttpOptions } from "../schema/index.js";
export declare class MissingCredentialError extends Error {
    readonly source: string;
    readonly _tag = "MissingCredentialError";
    constructor(source: string, message?: string);
}
export type CredentialError = MissingCredentialError | Config.ConfigError;
export type AuthError = CredentialError | AIError;
type Secret = string | Redacted.Redacted | Config.Config<string | Redacted.Redacted>;
export interface AuthInput {
    readonly request: {
        readonly http?: HttpOptions;
    };
    readonly method: "POST" | "GET" | "PUT" | "DELETE";
    readonly url: string;
    readonly body: string;
    readonly headers: Headers.Headers;
}
export interface Credential {
    readonly load: Effect.Effect<Redacted.Redacted, CredentialError>;
    readonly orElse: (that: Credential) => Credential;
    readonly bearer: () => Definition;
    readonly header: (name: string) => Definition;
    readonly pipe: <A>(f: (self: Credential) => A) => A;
}
export interface Definition {
    readonly apply: (input: AuthInput) => Effect.Effect<Headers.Headers, AuthError>;
    readonly andThen: (that: Definition) => Definition;
    readonly orElse: (that: Definition) => Definition;
    readonly pipe: <A>(f: (self: Definition) => A) => A;
}
export declare const isAuth: (input: unknown) => input is Definition;
export declare const value: (secret: string, source?: string) => Credential;
export declare const optional: (secret: Secret | undefined, source?: string) => Credential;
export declare const config: (name: string) => Credential;
export declare const effect: (load: Effect.Effect<Redacted.Redacted, CredentialError>) => Credential;
export declare const none: Definition;
export declare const headers: (input: Headers.Input) => Definition;
export declare const remove: (name: string) => Definition;
export declare const custom: (apply: (input: AuthInput) => Effect.Effect<Headers.Headers, AIError>) => Definition;
export declare const passthrough: Definition;
export declare function bearer(source: Secret | Credential): Definition;
export declare const apiKey: typeof bearer;
export declare function header(name: string): (source: Secret | Credential) => Definition;
export declare function header(name: string, source: Secret | Credential): Definition;
export declare function bearerHeader(name: string): (source: Secret | Credential) => Definition;
export declare function bearerHeader(name: string, source: Secret | Credential): Definition;
/** `Authorization: <scheme> <secret>` for providers whose scheme is not `Bearer`, such as fal's `Key`. */
export declare function scheme(name: string): (source: Secret | Credential) => Definition;
export declare function scheme(name: string, source: Secret | Credential): Definition;
export declare const toEffect: (input: Definition) => (authInput: AuthInput) => Effect.Effect<Headers.Headers, AIError>;
export * as Auth from "./auth.js";
