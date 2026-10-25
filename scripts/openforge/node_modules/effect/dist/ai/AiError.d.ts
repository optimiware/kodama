/**
 * Defines shared errors for AI operations.
 *
 * `AiError` records where a failure happened and stores the detailed reason in a
 * `reason` field. Those reasons cover transport problems, provider responses,
 * rate limits, authentication, content policy failures, invalid requests,
 * invalid output, unsupported schemas, tool failures, invalid user input, and
 * unknown failures. This module also includes metadata schemas, guards,
 * constructors, and helpers for converting HTTP response information into AI
 * error reasons.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Duration from "../Duration.ts";
import type * as HttpClientError from "../http/HttpClientError.ts";
import * as Schema from "../Schema.ts";
/**
 * Schema for HTTP requests to an AI provider.
 *
 * **Example** (Describing an HTTP request)
 *
 * ```ts import.meta.vitest
 * import type { AiError } from "effect/ai"
 *
 * const requestDetails: typeof AiError.HttpRequestDetails.Type = {
 *   method: "POST",
 *   url: "https://api.openai.com/v1/responses",
 *   urlParams: [],
 *   hash: undefined,
 *   headers: { "Content-Type": "application/json" }
 * }
 * const result = [requestDetails.method, requestDetails.urlParams] // => ["POST", []]
 * ```
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const HttpRequestDetails: Schema.Struct<{
    readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
    readonly url: Schema.String;
    readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
    readonly hash: Schema.optional<Schema.String>;
    readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
}>;
/**
 * Schema for HTTP responses from an AI provider.
 *
 * **Example** (Describing an HTTP response)
 *
 * ```ts import.meta.vitest
 * import type { AiError } from "effect/ai"
 *
 * const responseDetails: typeof AiError.HttpResponseDetails.Type = {
 *   status: 200,
 *   headers: {
 *     "Content-Type": "application/json",
 *     "X-Request-Id": "req_abc123"
 *   }
 * }
 * const result = [responseDetails.status, responseDetails.headers["X-Request-Id"]] // => [200, "req_abc123"]
 * ```
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const HttpResponseDetails: Schema.Struct<{
    readonly status: Schema.Int;
    readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
}>;
declare const ReasonTypeId: "~effect/ai/AiError/Reason";
declare const NetworkError_base: Schema.Class<NetworkError, Schema.Struct<{
    readonly _tag: Schema.tag<"NetworkError">;
    readonly reason: Schema.Literals<readonly ["TransportError", "EncodeError", "InvalidUrlError"]>;
    readonly request: Schema.Struct<{
        readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
        readonly url: Schema.String;
        readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
        readonly hash: Schema.optional<Schema.String>;
        readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
    }>;
    readonly description: Schema.optional<Schema.String>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating a network-level failure before receiving a response.
 *
 * **Details**
 *
 * This error is raised when issues arise before receiving an HTTP response,
 * such as network connectivity problems, request encoding issues, or invalid
 * URLs.
 *
 * **Example** (Creating a network error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.NetworkError({
 *   reason: "TransportError",
 *   request: {
 *     method: "POST",
 *     url: "https://api.openai.com/v1/completions",
 *     urlParams: [],
 *     hash: undefined,
 *     headers: { "Content-Type": "application/json" }
 *   },
 *   description: "Connection timeout after 30 seconds"
 * })
 *
 * const result = [error.reason, error.isRetryable] // => ["TransportError", true]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class NetworkError extends NetworkError_base {
    /**
     * Marks `NetworkError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Transport errors are retryable; encoding and URL errors are not.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    /**
     * Creates a NetworkError from a platform HttpClientError.RequestError.
     *
     * **Example** (Creating a network error from a request error)
     *
     * ```ts import.meta.vitest
     * import { AiError } from "effect/ai"
     * import { HttpClientError, HttpClientRequest } from "effect/http"
     *
     * const platformError = new HttpClientError.TransportError({
     *   request: HttpClientRequest.get("https://example.com/models"),
     *   description: "Connection refused"
     * })
     *
     * const aiError = AiError.NetworkError.fromRequestError(platformError)
     * aiError.reason // => "TransportError"
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    static fromRequestError(error: HttpClientError.RequestError): NetworkError;
    get message(): string;
}
/**
 * Schema for provider-specific metadata which can be attached to error reasons.
 *
 * **Details**
 *
 * Provider-specific metadata is namespaced by provider name. Each provider
 * value can contain arbitrary mutable JSON metadata or `null`.
 *
 * **Example** (Inspecting metadata shape)
 *
 * ```ts import.meta.vitest
 * const metadata = {
 *   openai: {
 *     errorCode: "rate_limit_exceeded",
 *     requestId: "req_123"
 *   },
 *   anthropic: null
 * }
 *
 * Array.of(metadata.openai.errorCode, metadata.anthropic) // => ["rate_limit_exceeded", null]
 * ```
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const ProviderMetadata: Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson>>>;
/**
 * Type of provider-specific metadata attached to AI error reasons.
 *
 * **Details**
 *
 * Metadata is keyed by provider name, and each provider value is either mutable
 * JSON metadata or `null`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ProviderMetadata = typeof ProviderMetadata.Type;
/**
 * Provider-specific metadata attached to `RateLimitError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface RateLimitErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `QuotaExhaustedError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface QuotaExhaustedErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `AuthenticationError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface AuthenticationErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `ContentPolicyError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface ContentPolicyErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `InvalidRequestError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface InvalidRequestErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `InternalProviderError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface InternalProviderErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `InvalidOutputError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface InvalidOutputErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `StructuredOutputError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface StructuredOutputErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `UnsupportedSchemaError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface UnsupportedSchemaErrorMetadata extends ProviderMetadata {
}
/**
 * Provider-specific metadata attached to `UnknownError`.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export interface UnknownErrorMetadata extends ProviderMetadata {
}
/**
 * Schema for token usage information from AI operations.
 *
 * **Details**
 *
 * Schema for optional provider-reported token counts for prompt tokens,
 * completion tokens, and total tokens.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const UsageInfo: Schema.Struct<{
    readonly promptTokens: Schema.optional<Schema.Int>;
    readonly completionTokens: Schema.optional<Schema.Int>;
    readonly totalTokens: Schema.optional<Schema.Int>;
}>;
/**
 * Schema for the combined HTTP context used in error reporting.
 *
 * **When to use**
 *
 * Use to attach request details, optional response details, and optional body
 * text to AI provider errors.
 *
 * **Details**
 *
 * Includes the required request details plus optional response details and raw
 * response body.
 *
 * @see {@link HttpRequestDetails} for captured request details
 * @see {@link HttpResponseDetails} for captured response details
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const HttpContext: Schema.Struct<{
    readonly request: Schema.Struct<{
        readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
        readonly url: Schema.String;
        readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
        readonly hash: Schema.optional<Schema.String>;
        readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
    }>;
    readonly response: Schema.optional<Schema.Struct<{
        readonly status: Schema.Int;
        readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
    }>>;
    readonly body: Schema.optional<Schema.String>;
}>;
/**
 * Builds a description for an HTTP error returned by an AI provider.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export declare const buildErrorDescription: (params: {
    readonly status: number;
    readonly message: string | undefined;
    readonly method: string;
    readonly url: string;
    readonly errorCode?: string | number | null | undefined;
    readonly errorType?: string | null | undefined;
    readonly requestId?: string | null | undefined;
    readonly body: string | undefined;
}) => string;
declare const RateLimitError_base: Schema.Class<RateLimitError, Schema.Struct<{
    readonly _tag: Schema.tag<"RateLimitError">;
    readonly retryAfter: Schema.optional<Schema.Duration>;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<RateLimitErrorMetadata>>, never>;
    readonly http: Schema.optional<Schema.Struct<{
        readonly request: Schema.Struct<{
            readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
            readonly url: Schema.String;
            readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
            readonly hash: Schema.optional<Schema.String>;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>;
        readonly response: Schema.optional<Schema.Struct<{
            readonly status: Schema.Int;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>>;
        readonly body: Schema.optional<Schema.String>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the request was rate limited.
 *
 * **Details**
 *
 * Rate limit errors are always retryable. When `retryAfter` is provided,
 * callers should wait that duration before retrying.
 *
 * **Example** (Creating a rate limit error)
 *
 * ```ts import.meta.vitest
 * import { Duration } from "effect"
 * import { AiError } from "effect/ai"
 *
 * const rateLimitError = new AiError.RateLimitError({
 *   retryAfter: Duration.seconds(60)
 * })
 *
 * const result = [rateLimitError._tag, rateLimitError.isRetryable] // => ["RateLimitError", true]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class RateLimitError extends RateLimitError_base {
    /**
     * Marks `RateLimitError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Rate limit errors are always retryable.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const QuotaExhaustedError_base: Schema.Class<QuotaExhaustedError, Schema.Struct<{
    readonly _tag: Schema.tag<"QuotaExhaustedError">;
    readonly resetAt: Schema.optional<Schema.DateTimeUtc>;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<QuotaExhaustedErrorMetadata>>, never>;
    readonly http: Schema.optional<Schema.Struct<{
        readonly request: Schema.Struct<{
            readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
            readonly url: Schema.String;
            readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
            readonly hash: Schema.optional<Schema.String>;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>;
        readonly response: Schema.optional<Schema.Struct<{
            readonly status: Schema.Int;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>>;
        readonly body: Schema.optional<Schema.String>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating account or billing limits have been reached.
 *
 * **Details**
 *
 * Quota exhausted errors are not retryable without user action.
 *
 * **Example** (Creating a quota exhausted error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const quotaError = new AiError.QuotaExhaustedError({})
 *
 * const result = [quotaError._tag, quotaError.isRetryable] // => ["QuotaExhaustedError", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class QuotaExhaustedError extends QuotaExhaustedError_base {
    /**
     * Marks `QuotaExhaustedError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Quota exhausted errors require user action and are not retryable.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const AuthenticationError_base: Schema.Class<AuthenticationError, Schema.Struct<{
    readonly _tag: Schema.tag<"AuthenticationError">;
    readonly kind: Schema.Literals<readonly ["InvalidKey", "ExpiredKey", "MissingKey", "InsufficientPermissions", "Unknown"]>;
    readonly description: Schema.optional<Schema.String>;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<AuthenticationErrorMetadata>>, never>;
    readonly http: Schema.optional<Schema.Struct<{
        readonly request: Schema.Struct<{
            readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
            readonly url: Schema.String;
            readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
            readonly hash: Schema.optional<Schema.String>;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>;
        readonly response: Schema.optional<Schema.Struct<{
            readonly status: Schema.Int;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>>;
        readonly body: Schema.optional<Schema.String>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating authentication or authorization failure.
 *
 * **Details**
 *
 * Authentication errors are never retryable without credential changes.
 *
 * **Example** (Creating an authentication error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const authError = new AiError.AuthenticationError({
 *   kind: "InvalidKey"
 * })
 *
 * const result = [authError.kind, authError.isRetryable] // => ["InvalidKey", false]
 *
 * const detailed = new AiError.AuthenticationError({
 *   kind: "InsufficientPermissions",
 *   description: "Token expired"
 * })
 *
 * detailed.message // => "InsufficientPermissions: Your API key lacks required permissions. Token expired"
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class AuthenticationError extends AuthenticationError_base {
    /**
     * Marks `AuthenticationError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Authentication errors require credential changes and are not retryable.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const ContentPolicyError_base: Schema.Class<ContentPolicyError, Schema.Struct<{
    readonly _tag: Schema.tag<"ContentPolicyError">;
    readonly description: Schema.String;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<ContentPolicyErrorMetadata>>, never>;
    readonly http: Schema.optional<Schema.Struct<{
        readonly request: Schema.Struct<{
            readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
            readonly url: Schema.String;
            readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
            readonly hash: Schema.optional<Schema.String>;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>;
        readonly response: Schema.optional<Schema.Struct<{
            readonly status: Schema.Int;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>>;
        readonly body: Schema.optional<Schema.String>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating content policy violation.
 *
 * **Details**
 *
 * Content policy errors are never retryable without content changes.
 *
 * **Example** (Creating a content policy error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const policyError = new AiError.ContentPolicyError({
 *   description: "Input contains prohibited content"
 * })
 *
 * const result = [policyError.description, policyError.isRetryable] // => ["Input contains prohibited content", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ContentPolicyError extends ContentPolicyError_base {
    /**
     * Marks `ContentPolicyError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Content policy errors require content changes and are not retryable.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const InvalidRequestError_base: Schema.Class<InvalidRequestError, Schema.Struct<{
    readonly _tag: Schema.tag<"InvalidRequestError">;
    readonly parameter: Schema.optional<Schema.String>;
    readonly constraint: Schema.optional<Schema.String>;
    readonly description: Schema.optional<Schema.String>;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<InvalidRequestErrorMetadata>>, never>;
    readonly http: Schema.optional<Schema.Struct<{
        readonly request: Schema.Struct<{
            readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
            readonly url: Schema.String;
            readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
            readonly hash: Schema.optional<Schema.String>;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>;
        readonly response: Schema.optional<Schema.Struct<{
            readonly status: Schema.Int;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>>;
        readonly body: Schema.optional<Schema.String>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the request had invalid or malformed parameters.
 *
 * **Details**
 *
 * Invalid request errors require fixing the request and are not retryable.
 *
 * **Example** (Creating an invalid request error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const invalidRequestError = new AiError.InvalidRequestError({
 *   parameter: "temperature",
 *   constraint: "must be between 0 and 2",
 *   description: "Temperature value 5 is out of range"
 * })
 *
 * const result = [invalidRequestError.parameter, invalidRequestError.isRetryable] // => ["temperature", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidRequestError extends InvalidRequestError_base {
    /**
     * Marks `InvalidRequestError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Invalid request errors require fixing the request and are not retryable.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const InternalProviderError_base: Schema.Class<InternalProviderError, Schema.Struct<{
    readonly _tag: Schema.tag<"InternalProviderError">;
    readonly description: Schema.String;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<InternalProviderErrorMetadata>>, never>;
    readonly http: Schema.optional<Schema.Struct<{
        readonly request: Schema.Struct<{
            readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
            readonly url: Schema.String;
            readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
            readonly hash: Schema.optional<Schema.String>;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>;
        readonly response: Schema.optional<Schema.Struct<{
            readonly status: Schema.Int;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>>;
        readonly body: Schema.optional<Schema.String>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the AI provider experienced an internal error.
 *
 * **Details**
 *
 * Internal provider errors are typically transient and are retryable.
 *
 * **Example** (Creating an internal provider error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const providerError = new AiError.InternalProviderError({
 *   description: "Server encountered an unexpected error"
 * })
 *
 * const result = [providerError.description, providerError.isRetryable] // => ["Server encountered an unexpected error", true]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InternalProviderError extends InternalProviderError_base {
    /**
     * Marks `InternalProviderError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Internal provider errors are typically transient and are retryable.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const InvalidOutputError_base: Schema.Class<InvalidOutputError, Schema.Struct<{
    readonly _tag: Schema.tag<"InvalidOutputError">;
    readonly description: Schema.String;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<InvalidOutputErrorMetadata>>, never>;
    readonly usage: Schema.optional<Schema.Struct<{
        readonly promptTokens: Schema.optional<Schema.Int>;
        readonly completionTokens: Schema.optional<Schema.Int>;
        readonly totalTokens: Schema.optional<Schema.Int>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating failure to parse or validate LLM output.
 *
 * **Details**
 *
 * Invalid output errors are retryable since LLM outputs are non-deterministic.
 *
 * **Example** (Creating an invalid output error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const parseError = new AiError.InvalidOutputError({
 *   description: "Expected a string but received a number"
 * })
 *
 * const result = [parseError.description, parseError.isRetryable] // => ["Expected a string but received a number", true]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidOutputError extends InvalidOutputError_base {
    /**
     * Marks `InvalidOutputError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Invalid output errors are retryable since LLM outputs are non-deterministic.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    /**
     * Creates an InvalidOutputError from a Schema error.
     *
     * **Example** (Creating an invalid output error from a schema error)
     *
     * ```ts import.meta.vitest
     * import { Effect, Schema } from "effect"
     * import { AiError } from "effect/ai"
     *
     * const schemaError = await Effect.runPromise(
     *   Schema.decodeUnknownEffect(Schema.Number)("not a number").pipe(Effect.flip)
     * )
     * const parseError = AiError.InvalidOutputError.fromSchemaError(schemaError)
     * parseError.description // => "Expected number"
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    static fromSchemaError(error: Schema.SchemaError): InvalidOutputError;
    get message(): string;
}
declare const StructuredOutputError_base: Schema.Class<StructuredOutputError, Schema.Struct<{
    readonly _tag: Schema.tag<"StructuredOutputError">;
    readonly description: Schema.String;
    readonly responseText: Schema.String;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<StructuredOutputErrorMetadata>>, never>;
    readonly usage: Schema.optional<Schema.Struct<{
        readonly promptTokens: Schema.optional<Schema.Int>;
        readonly completionTokens: Schema.optional<Schema.Int>;
        readonly totalTokens: Schema.optional<Schema.Int>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the LLM generated text that does not conform to the
 * requested structured output schema.
 *
 * **Details**
 *
 * Structured output errors are retryable since LLM outputs are non-deterministic.
 *
 * **Example** (Creating a structured output error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.StructuredOutputError({
 *   description: "Expected a valid JSON object",
 *   responseText: "{\"foo\":}"
 * })
 *
 * const result = [error.description, error.responseText, error.isRetryable] // => ["Expected a valid JSON object", '{"foo":}', true]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class StructuredOutputError extends StructuredOutputError_base {
    /**
     * Marks `StructuredOutputError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Structured output errors are retryable since LLM outputs are non-deterministic.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    /**
     * Creates a StructuredOutputError from a Schema error.
     *
     * **Example** (Creating a structured output error from a schema error)
     *
     * ```ts import.meta.vitest
     * import { Effect, Schema } from "effect"
     * import { AiError } from "effect/ai"
     *
     * const schemaError = await Effect.runPromise(
     *   Schema.decodeUnknownEffect(Schema.Struct({ name: Schema.String }))({}).pipe(Effect.flip)
     * )
     * const parseError = AiError.StructuredOutputError.fromSchemaError(schemaError, "{}")
     * parseError.responseText // => "{}"
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    static fromSchemaError(error: Schema.SchemaError, responseText: string): StructuredOutputError;
    get message(): string;
}
declare const UnsupportedSchemaError_base: Schema.Class<UnsupportedSchemaError, Schema.Struct<{
    readonly _tag: Schema.tag<"UnsupportedSchemaError">;
    readonly description: Schema.String;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<UnsupportedSchemaErrorMetadata>>, never>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating a codec transformer rejected a schema because it contains
 * unsupported constructs.
 *
 * **Details**
 *
 * Unsupported schema errors are not retryable because they indicate a
 * programmer error where the schema is incompatible with the provider.
 *
 * **Example** (Creating an unsupported schema error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.UnsupportedSchemaError({
 *   description: "Unions are not supported in Anthropic structured output"
 * })
 *
 * const result = [error.description, error.isRetryable] // => ["Unions are not supported in Anthropic structured output", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class UnsupportedSchemaError extends UnsupportedSchemaError_base {
    /**
     * Marks `UnsupportedSchemaError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Unsupported schema errors are not retryable because they indicate a programmer error.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const UnknownError_base: Schema.Class<UnknownError, Schema.Struct<{
    readonly _tag: Schema.tag<"UnknownError">;
    readonly description: Schema.optional<Schema.String>;
    readonly metadata: Schema.withDecodingDefault<Schema.withConstructorDefault<Schema.$Record<Schema.String, Schema.NullOr<Schema.Codec<Schema.MutableJson, Schema.MutableJson, never, never>>> & Schema.Schema<UnknownErrorMetadata>>, never>;
    readonly http: Schema.optional<Schema.Struct<{
        readonly request: Schema.Struct<{
            readonly method: Schema.Literals<readonly ["GET", "POST", "PATCH", "PUT", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY"]>;
            readonly url: Schema.String;
            readonly urlParams: Schema.$Array<Schema.Tuple<readonly [Schema.String, Schema.String]>>;
            readonly hash: Schema.optional<Schema.String>;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>;
        readonly response: Schema.optional<Schema.Struct<{
            readonly status: Schema.Int;
            readonly headers: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Redacted<Schema.String>]>>;
        }>>;
        readonly body: Schema.optional<Schema.String>;
    }>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error data for unknown or unexpected AI failures.
 *
 * **Details**
 *
 * Unknown errors are not retryable by default since the cause is unknown.
 *
 * **Example** (Creating an unknown error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const unknownError = new AiError.UnknownError({
 *   description: "An unexpected error occurred"
 * })
 *
 * const result = [unknownError.description, unknownError.isRetryable] // => ["An unexpected error occurred", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class UnknownError extends UnknownError_base {
    /**
     * Marks `UnknownError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Unknown errors are not retryable by default.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const ToolNotFoundError_base: Schema.Class<ToolNotFoundError, Schema.Struct<{
    readonly _tag: Schema.tag<"ToolNotFoundError">;
    readonly toolName: Schema.String;
    readonly availableTools: Schema.$Array<Schema.String>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the model requested a tool that doesn't exist in the toolkit.
 *
 * **Details**
 *
 * This error is retryable because the model may self-correct when provided
 * with the list of available tools.
 *
 * **Example** (Creating a tool not found error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.ToolNotFoundError({
 *   toolName: "unknownTool",
 *   availableTools: ["GetWeather", "GetTime"]
 * })
 *
 * const result = [error.toolName, error.availableTools, error.isRetryable] // => ["unknownTool", ["GetWeather", "GetTime"], true]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ToolNotFoundError extends ToolNotFoundError_base {
    /**
     * Marks `ToolNotFoundError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Tool not found errors are retryable because the model may self-correct.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const ToolParameterValidationError_base: Schema.Class<ToolParameterValidationError, Schema.Struct<{
    readonly _tag: Schema.tag<"ToolParameterValidationError">;
    readonly toolName: Schema.String;
    readonly description: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the model's tool call parameters failed schema validation.
 *
 * **Details**
 *
 * This error is retryable because the model may correct its parameters
 * on subsequent attempts.
 *
 * **Example** (Creating a tool parameter validation error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.ToolParameterValidationError({
 *   toolName: "GetWeather",
 *   description: "Expected string, got number"
 * })
 *
 * const result = [error.toolName, error.description, error.isRetryable] // => ["GetWeather", "Expected string, got number", true]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ToolParameterValidationError extends ToolParameterValidationError_base {
    /**
     * Marks `ToolParameterValidationError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Parameter validation errors are retryable because the model may correct parameters.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const InvalidToolResultError_base: Schema.Class<InvalidToolResultError, Schema.Struct<{
    readonly _tag: Schema.tag<"InvalidToolResultError">;
    readonly toolName: Schema.String;
    readonly description: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the tool handler returned an invalid result that does not
 * match the tool's schema.
 *
 * **Details**
 *
 * This error is not retryable because invalid results indicate a bug in the
 * tool handler implementation.
 *
 * **Example** (Creating an invalid tool result error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.InvalidToolResultError({
 *   toolName: "GetWeather",
 *   description: "Tool handler returned invalid result: missing 'temperature' field"
 * })
 *
 * const result = [error.toolName, error.isRetryable] // => ["GetWeather", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidToolResultError extends InvalidToolResultError_base {
    /**
     * Marks `InvalidToolResultError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Invalid tool result errors are not retryable because they indicate a bug in the handler.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const ToolResultEncodingError_base: Schema.Class<ToolResultEncodingError, Schema.Struct<{
    readonly _tag: Schema.tag<"ToolResultEncodingError">;
    readonly toolName: Schema.String;
    readonly toolResult: Schema.Unknown;
    readonly description: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the tool result cannot be encoded for sending back to the model.
 *
 * **Details**
 *
 * This error is not retryable because encoding failures indicate a bug in the
 * tool schema definitions.
 *
 * **Example** (Creating a tool result encoding error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.ToolResultEncodingError({
 *   toolName: "GetWeather",
 *   toolResult: { temperature: 72n },
 *   description: "Cannot encode bigint values as JSON"
 * })
 *
 * const result = [error.toolName, error.description, error.isRetryable] // => ["GetWeather", "Cannot encode bigint values as JSON", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ToolResultEncodingError extends ToolResultEncodingError_base {
    /**
     * Marks `ToolResultEncodingError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Encoding errors are not retryable because they indicate a code bug.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const ToolConfigurationError_base: Schema.Class<ToolConfigurationError, Schema.Struct<{
    readonly _tag: Schema.tag<"ToolConfigurationError">;
    readonly toolName: Schema.String;
    readonly description: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating a provider-defined tool was configured with invalid arguments.
 *
 * **Details**
 *
 * This error is not retryable because it indicates a programming error in the
 * tool configuration that must be fixed in code.
 *
 * **Example** (Creating a tool configuration error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.ToolConfigurationError({
 *   toolName: "OpenAiCodeInterpreter",
 *   description: "Invalid container ID format"
 * })
 *
 * const result = [error.toolName, error.description, error.isRetryable] // => ["OpenAiCodeInterpreter", "Invalid container ID format", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ToolConfigurationError extends ToolConfigurationError_base {
    /**
     * Marks `ToolConfigurationError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Configuration errors are not retryable because they indicate a code bug.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const ToolkitRequiredError_base: Schema.Class<ToolkitRequiredError, Schema.Struct<{
    readonly _tag: Schema.tag<"ToolkitRequiredError">;
    readonly pendingApprovals: Schema.$Array<Schema.String>;
    readonly description: Schema.optional<Schema.String>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating an operation requires a toolkit but none was provided.
 *
 * **Details**
 *
 * This error occurs when tool approval responses are present in the prompt
 * but no toolkit was provided to resolve them.
 *
 * **Example** (Creating a toolkit required error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.ToolkitRequiredError({
 *   pendingApprovals: ["GetWeather", "SendEmail"]
 * })
 *
 * const result = [error.pendingApprovals, error.isRetryable] // => [["GetWeather", "SendEmail"], false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ToolkitRequiredError extends ToolkitRequiredError_base {
    /**
     * Marks `ToolkitRequiredError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Toolkit required errors are not retryable without providing a toolkit.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
declare const InvalidUserInputError_base: Schema.Class<InvalidUserInputError, Schema.Struct<{
    readonly _tag: Schema.tag<"InvalidUserInputError">;
    readonly description: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error indicating the user provided invalid input in their prompt.
 *
 * **Details**
 *
 * This error is raised when the prompt contains content that is structurally
 * valid but not supported by the provider (e.g., unsupported media types,
 * unsupported file formats, etc.).
 *
 * **Example** (Creating an invalid user input error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const error = new AiError.InvalidUserInputError({
 *   description: "Unsupported media type 'video/mp4'. Supported types include images, application/pdf, text/plain"
 * })
 *
 * const result = [error._tag, error.isRetryable] // => ["InvalidUserInputError", false]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidUserInputError extends InvalidUserInputError_base {
    /**
     * Marks `InvalidUserInputError` as a semantic AI error reason for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ReasonTypeId]: "~effect/ai/AiError/Reason";
    /**
     * Invalid user input errors require fixing the input and are not retryable.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    get message(): string;
}
/**
 * Union type of all semantic error reasons that can occur during AI operations.
 *
 * **Details**
 *
 * Every reason carries a semantic `_tag`, a human-readable message, and an
 * `isRetryable` getter. Provider-facing reasons may also include retry timing,
 * provider metadata, usage information, or HTTP context.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type AiErrorReason = RateLimitError | QuotaExhaustedError | AuthenticationError | ContentPolicyError | InvalidRequestError | InternalProviderError | NetworkError | InvalidOutputError | StructuredOutputError | UnsupportedSchemaError | UnknownError | ToolNotFoundError | ToolParameterValidationError | InvalidToolResultError | ToolResultEncodingError | ToolConfigurationError | ToolkitRequiredError | InvalidUserInputError;
/**
 * Schema for validating and parsing AI error reasons.
 *
 * **When to use**
 *
 * Use when decoding or validating unknown AI error reason values with Schema.
 *
 * **Details**
 *
 * This runtime schema is the union of the concrete AI error reason classes.
 *
 * @see {@link isAiErrorReason} for checking an existing value without Schema decoding
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const AiErrorReason: Schema.Union<[
    typeof RateLimitError,
    typeof QuotaExhaustedError,
    typeof AuthenticationError,
    typeof ContentPolicyError,
    typeof InvalidRequestError,
    typeof InternalProviderError,
    typeof NetworkError,
    typeof InvalidOutputError,
    typeof StructuredOutputError,
    typeof UnsupportedSchemaError,
    typeof UnknownError,
    typeof ToolNotFoundError,
    typeof ToolParameterValidationError,
    typeof InvalidToolResultError,
    typeof ToolResultEncodingError,
    typeof ToolConfigurationError,
    typeof ToolkitRequiredError,
    typeof InvalidUserInputError
]>;
declare const TypeId: "~effect/ai/AiError";
declare const AiError_base: Schema.Class<AiError, Schema.Struct<{
    readonly _tag: Schema.tag<"AiError">;
    readonly module: Schema.String;
    readonly method: Schema.String;
    readonly reason: Schema.Union<[typeof RateLimitError, typeof QuotaExhaustedError, typeof AuthenticationError, typeof ContentPolicyError, typeof InvalidRequestError, typeof InternalProviderError, typeof NetworkError, typeof InvalidOutputError, typeof StructuredOutputError, typeof UnsupportedSchemaError, typeof UnknownError, typeof ToolNotFoundError, typeof ToolParameterValidationError, typeof InvalidToolResultError, typeof ToolResultEncodingError, typeof ToolConfigurationError, typeof ToolkitRequiredError, typeof InvalidUserInputError]>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Schema for the top-level AI error wrapper using the `reason` pattern.
 *
 * **When to use**
 *
 * Use when you need AI errors that can be handled by semantic reason with
 * `Effect.catchReason`.
 *
 * **Details**
 *
 * This error stores `module` and `method` context, the semantic `reason`, and
 * delegates `isRetryable` and `retryAfter` to the underlying reason.
 *
 * **Example** (Handling an AI error by tag)
 *
 * ```ts import.meta.vitest
 * import { Duration, Effect } from "effect"
 * import { AiError } from "effect/ai"
 *
 * const aiOperation = Effect.fail(new AiError.AiError({
 *   module: "OpenAI",
 *   method: "generateText",
 *   reason: new AiError.RateLimitError({ retryAfter: Duration.seconds(30) })
 * }))
 *
 * // Handle specific reason types
 * const handled = aiOperation.pipe(
 *   Effect.catchTag("AiError", (error) => {
 *     if (error.reason._tag === "RateLimitError") {
 *       return Effect.succeed(`Retry after ${error.retryAfter}`)
 *     }
 *     return Effect.fail(error)
 *   })
 * )
 *
 * await Effect.runPromise(handled) // => "Retry after 30000 millis"
 * ```
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class AiError extends AiError_base {
    readonly [TypeId]: "~effect/ai/AiError";
    readonly cause: AuthenticationError | ContentPolicyError | InternalProviderError | InvalidOutputError | InvalidRequestError | InvalidToolResultError | InvalidUserInputError | NetworkError | QuotaExhaustedError | RateLimitError | StructuredOutputError | ToolConfigurationError | ToolNotFoundError | ToolParameterValidationError | ToolResultEncodingError | ToolkitRequiredError | UnknownError | UnsupportedSchemaError;
    /**
     * Delegates to the underlying reason's `isRetryable` getter.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get isRetryable(): boolean;
    /**
     * Delegates to the underlying reason's `retryAfter` if present.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get retryAfter(): Duration.Duration | undefined;
    get message(): string;
}
/**
 * The encoded (serialized) form of an `AiError`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export type AiErrorEncoded = typeof AiError["Encoded"];
/**
 * Type guard to check if a value is an `AiError`.
 *
 * **Example** (Checking for an AI error)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const someError = new Error("generic error")
 * const aiError = AiError.make({
 *   module: "Test",
 *   method: "example",
 *   reason: new AiError.RateLimitError({})
 * })
 *
 * const result = [AiError.isAiError(someError), AiError.isAiError(aiError)] // => [false, true]
 * ```
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isAiError: (u: unknown) => u is AiError;
/**
 * Type guard to check if a value is an `AiErrorReason`.
 *
 * **Example** (Checking for an AI error reason)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const rateLimitError = new AiError.RateLimitError({})
 * const genericError = new Error("generic error")
 *
 * const result = [AiError.isAiErrorReason(rateLimitError), AiError.isAiErrorReason(genericError)] // => [true, false]
 * ```
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isAiErrorReason: (u: unknown) => u is AiErrorReason;
/**
 * Creates an `AiError` with the given reason.
 *
 * **Example** (Creating an AI error)
 *
 * ```ts import.meta.vitest
 * import { Duration } from "effect"
 * import { AiError } from "effect/ai"
 *
 * const error = AiError.make({
 *   module: "OpenAI",
 *   method: "completion",
 *   reason: new AiError.RateLimitError({
 *     retryAfter: Duration.seconds(60)
 *   })
 * })
 *
 * const result = [error.module, error.method, error.reason._tag] // => ["OpenAI", "completion", "RateLimitError"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (params: {
    readonly module: string;
    readonly method: string;
    readonly reason: AiErrorReason;
}) => AiError;
/**
 * Maps HTTP status codes to semantic error reasons.
 *
 * **When to use**
 *
 * Use as the base mapping when provider packages translate HTTP status codes into
 * provider-specific error reasons.
 *
 * **Example** (Mapping an HTTP status to a reason)
 *
 * ```ts import.meta.vitest
 * import { AiError } from "effect/ai"
 *
 * const reason = AiError.reasonFromHttpStatus({
 *   status: 429,
 *   body: { error: "Rate limit exceeded" }
 * })
 *
 * reason._tag // => "RateLimitError"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const reasonFromHttpStatus: (params: {
    readonly status: number;
    readonly body?: unknown;
    readonly http?: typeof HttpContext.Type;
    readonly metadata?: ProviderMetadata;
    readonly description?: string | undefined;
}) => AiErrorReason;
export {};
//# sourceMappingURL=AiError.d.ts.map