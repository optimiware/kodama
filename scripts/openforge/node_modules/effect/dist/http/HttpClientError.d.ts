import * as Schema from "../Schema.ts";
import type * as HttpClientRequest from "./HttpClientRequest.ts";
import type * as ClientResponse from "./HttpClientResponse.ts";
declare const TypeId = "~effect/http/HttpClientError";
/**
 * Returns `true` when a value is an `HttpClientError`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isHttpClientError: (u: unknown) => u is HttpClientError;
declare const HttpClientError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "HttpClientError";
} & Readonly<A>;
/**
 * Error wrapper for HTTP client failures, exposing the failed request and the optional response through its `reason`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class HttpClientError extends HttpClientError_base<{
    readonly reason: HttpClientErrorReason;
}> {
    constructor(props: {
        readonly reason: HttpClientErrorReason;
    });
    /**
     * Marks this value as an HTTP client error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/http/HttpClientError";
    /**
     * HTTP request associated with the client failure.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get request(): HttpClientRequest.HttpClientRequest;
    /**
     * HTTP response associated with the client failure, when one was received.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get response(): ClientResponse.HttpClientResponse | undefined;
    get message(): string;
}
declare const TransportError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "TransportError";
} & Readonly<A>;
/**
 * Error describing transport-level failures that occur while sending an HTTP request.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class TransportError extends TransportError_base<{
    readonly request: HttpClientRequest.HttpClientRequest;
    readonly cause?: unknown;
    readonly description?: string;
}> {
    /**
     * Formats the request method and URL for transport error messages.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get methodAndUrl(): string;
    /**
     * Builds the transport error message from the optional description and request details.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const EncodeError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "EncodeError";
} & Readonly<A>;
/**
 * Error describing failures while encoding an HTTP request body.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class EncodeError extends EncodeError_base<{
    readonly request: HttpClientRequest.HttpClientRequest;
    readonly cause?: unknown;
    readonly description?: string;
}> {
    /**
     * Formats the request method and URL for request encoding error messages.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get methodAndUrl(): string;
    /**
     * Builds the request encoding error message from the optional description and request details.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const InvalidUrlError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "InvalidUrlError";
} & Readonly<A>;
/**
 * Error describing failures while constructing a URL from an HTTP client request.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidUrlError extends InvalidUrlError_base<{
    readonly request: HttpClientRequest.HttpClientRequest;
    readonly cause?: unknown;
    readonly description?: string;
}> {
    /**
     * Formats the request method and URL for invalid URL error messages.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get methodAndUrl(): string;
    /**
     * Builds the invalid URL error message from the optional description and request details.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const StatusCodeError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "StatusCodeError";
} & Readonly<A>;
/**
 * Response error for HTTP responses rejected because of their status code.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class StatusCodeError extends StatusCodeError_base<{
    readonly request: HttpClientRequest.HttpClientRequest;
    readonly response: ClientResponse.HttpClientResponse;
    readonly cause?: unknown;
    readonly description?: string | undefined;
}> {
    /**
     * Formats the request method and URL for status code error messages.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get methodAndUrl(): string;
    /**
     * Builds the status code error message from the response status, optional description, and request details.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const DecodeError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "DecodeError";
} & Readonly<A>;
/**
 * Response error for failures while decoding an HTTP response body.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class DecodeError extends DecodeError_base<{
    readonly request: HttpClientRequest.HttpClientRequest;
    readonly response: ClientResponse.HttpClientResponse;
    readonly cause?: unknown;
    readonly description?: string | undefined;
}> {
    /**
     * Formats the request method and URL for response decoding error messages.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get methodAndUrl(): string;
    /**
     * Builds the response decoding error message from the response status, optional description, and request details.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const EmptyBodyError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "EmptyBodyError";
} & Readonly<A>;
/**
 * Response error for operations that expected a response body but received an empty body.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class EmptyBodyError extends EmptyBodyError_base<{
    readonly request: HttpClientRequest.HttpClientRequest;
    readonly response: ClientResponse.HttpClientResponse;
    readonly cause?: unknown;
    readonly description?: string | undefined;
}> {
    /**
     * Formats the request method and URL for empty response body error messages.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get methodAndUrl(): string;
    /**
     * Builds the empty body error message from the response status, optional description, and request details.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
/**
 * Union of HTTP client errors that occur before a response is available.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type RequestError = TransportError | EncodeError | InvalidUrlError;
/**
 * Union of HTTP client errors that include an HTTP response.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type ResponseError = StatusCodeError | DecodeError | EmptyBodyError;
/**
 * Union of all specific failure reasons carried by `HttpClientError`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type HttpClientErrorReason = RequestError | ResponseError;
declare const HttpClientErrorSchema_base: Schema.Class<HttpClientErrorSchema, Schema.Struct<{
    readonly _tag: Schema.tag<"HttpError">;
    readonly kind: Schema.Literals<("DecodeError" | "EmptyBodyError" | "EncodeError" | "InvalidUrlError" | "StatusCodeError" | "TransportError")[]>;
    readonly cause: Schema.optional<Schema.Defect>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Schema for serializable HTTP client errors, preserving the specific error kind
 * and cause.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class HttpClientErrorSchema extends HttpClientErrorSchema_base {
    /**
     * Builds the serializable schema representation for an HTTP client error.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static fromHttpClientError(error: HttpClientError): HttpClientErrorSchema;
}
export {};
//# sourceMappingURL=HttpClientError.d.ts.map