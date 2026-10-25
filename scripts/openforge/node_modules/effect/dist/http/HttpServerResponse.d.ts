/**
 * Describes immutable responses returned by Effect HTTP handlers.
 *
 * An `HttpServerResponse` stores the status, optional status text, headers,
 * cookies, and body that the server runtime later turns into a platform
 * response such as a Web `Response`. This module includes constructors for
 * common response bodies, helpers for updating response data, file response
 * support through `HttpPlatform`, and conversions to or from Web and Effect
 * HTTP client responses.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type * as ByteSize from "../ByteSize.ts";
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as ErrorReporter from "../ErrorReporter.ts";
import * as Inspectable from "../Inspectable.ts";
import { type Pipeable } from "../Pipeable.ts";
import type { PlatformError } from "../PlatformError.ts";
import type * as Schema from "../Schema.ts";
import type { ParseOptions } from "../SchemaAST.ts";
import * as Stream from "../Stream.ts";
import * as Cookies from "./Cookies.ts";
import * as Headers from "./Headers.ts";
import * as Body from "./HttpBody.ts";
import * as HttpClientRequest from "./HttpClientRequest.ts";
import * as HttpClientResponse from "./HttpClientResponse.ts";
import type { HttpPlatform } from "./HttpPlatform.ts";
import * as Template from "./Template.ts";
import * as UrlParams from "./UrlParams.ts";
declare const TypeId = "~effect/http/HttpServerResponse";
/**
 * Server-side HTTP response model.
 *
 * **Details**
 *
 * A response contains a status, optional status text, headers, cookies, and an
 * HTTP body that can later be converted to platform-specific response types.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface HttpServerResponse extends Inspectable.Inspectable, Pipeable, ErrorReporter.Reportable {
    readonly [TypeId]: typeof TypeId;
    readonly status: number;
    readonly statusText?: string | undefined;
    readonly headers: Headers.Headers;
    readonly cookies: Cookies.Cookies;
    readonly body: Body.HttpBody;
}
/**
 * Common options accepted by HTTP server response constructors.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface Options {
    readonly status?: number | undefined;
    readonly statusText?: string | undefined;
    readonly headers?: Headers.Input | undefined;
    readonly cookies?: Cookies.Cookies | undefined;
    readonly contentType?: string | undefined;
    readonly contentLength?: number | undefined;
}
/**
 * Option variants used by response constructors with different body metadata
 * rules.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Options {
    /**
     * Response options for constructors whose body determines its own content type
     * and content length.
     *
     * @stability unstable
     * @category options
     * @since 4.0.0
     */
    interface WithContent extends Omit<Options, "contentType" | "contentLength"> {
    }
    /**
     * Response options for constructors that allow overriding the content type while
     * deriving the content length from the body.
     *
     * @stability unstable
     * @category options
     * @since 4.0.0
     */
    interface WithContentType extends Omit<Options, "contentLength"> {
    }
}
/**
 * Returns `true` when the supplied value is an `HttpServerResponse`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isHttpServerResponse: (u: unknown) => u is HttpServerResponse;
/**
 * Creates an empty HTTP response.
 *
 * **Details**
 *
 * The default status is `204`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const empty: (options?: Options.WithContent | undefined) => HttpServerResponse;
/**
 * Creates a redirect response with a `Location` header.
 *
 * **Details**
 *
 * The default status is `302`; custom headers are merged with the generated
 * `Location` header.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const redirect: (location: string | URL, options?: Options.WithContent | undefined) => HttpServerResponse;
/**
 * Creates an HTTP response whose body is a `Uint8Array`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const uint8Array: (body: Uint8Array, options?: Options.WithContentType) => HttpServerResponse;
/**
 * Creates an HTTP response whose body is a string.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const text: (body: string, options?: Options.WithContentType) => HttpServerResponse;
/**
 * Creates an HTML response with the `text/html` content type.
 *
 * **Details**
 *
 * Passing a string returns a response directly. Using it as a template tag returns
 * an effect so interpolated values can be rendered with their required services
 * and errors.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const html: {
    /**
     * Creates an HTML response with the `text/html` content type.
     *
     * **Details**
     *
     * Passing a string returns a response directly. Using it as a template tag returns
     * an effect so interpolated values can be rendered with their required services
     * and errors.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <A extends ReadonlyArray<Template.Interpolated>>(strings: TemplateStringsArray, ...args: A): Effect.Effect<HttpServerResponse, Template.Interpolated.Error<A[number]>, Template.Interpolated.Context<A[number]>>;
    /**
     * Creates an HTML response with the `text/html` content type.
     *
     * **Details**
     *
     * Passing a string returns a response directly. Using it as a template tag returns
     * an effect so interpolated values can be rendered with their required services
     * and errors.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    (html: string): HttpServerResponse;
};
/**
 * Creates a streaming HTML response from a template.
 *
 * **Details**
 *
 * The template is encoded as a byte stream and can use streaming interpolated
 * values from the current context.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const htmlStream: <A extends ReadonlyArray<Template.InterpolatedWithStream>>(strings: TemplateStringsArray, ...args: A) => Effect.Effect<HttpServerResponse, never, Template.Interpolated.Context<A[number]>>;
/**
 * Creates a JSON HTTP response.
 *
 * **Details**
 *
 * The body is serialized with `JSON.stringify`; serialization errors are captured
 * as `HttpBodyError` failures.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const json: (body: unknown, options?: Options.WithContentType | undefined) => Effect.Effect<HttpServerResponse, Body.HttpBodyError>;
/**
 * Creates a JSON response constructor backed by a schema encoder.
 *
 * **Details**
 *
 * The returned function encodes the value with the supplied schema before
 * serializing it as JSON, and can fail with `HttpBodyError` if schema encoding or
 * JSON serialization fails.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const schemaJson: <A, RE>(schema: Schema.ConstraintCodec<A, unknown, unknown, RE>, options?: ParseOptions | undefined) => (body: A, options?: Options.WithContentType | undefined) => Effect.Effect<HttpServerResponse, Body.HttpBodyError, RE>;
/**
 * Creates a JSON HTTP response synchronously.
 *
 * **When to use**
 *
 * Use when the response body is known to be JSON-serializable and you need a
 * synchronous `HttpServerResponse`.
 *
 * **Gotchas**
 *
 * Unlike `json`, serialization errors from `JSON.stringify` are not captured in
 * `Effect`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const jsonUnsafe: (body: unknown, options?: Options.WithContentType | undefined) => HttpServerResponse;
/**
 * Creates a response from URL parameters using the
 * `application/x-www-form-urlencoded` content type by default.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const urlParams: (body: UrlParams.Input, options?: Options.WithContentType | undefined) => HttpServerResponse;
/**
 * Creates a response with a raw body value.
 *
 * **When to use**
 *
 * Use when you want to pass through a body value already understood by the
 * underlying runtime, such as a Web `Response`, `Blob`, or `ReadableStream`,
 * for later platform conversion.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const raw: (body: unknown, options?: Options | undefined) => HttpServerResponse;
/**
 * Creates a response whose body is a Web `FormData` value.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const formData: (body: FormData, options?: Options.WithContent | undefined) => HttpServerResponse;
/**
 * Creates a streaming response from a stream of byte chunks.
 *
 * **Details**
 *
 * Optional response metadata can supply the status, headers, content type, and
 * content length.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const stream: <E>(body: Stream.Stream<Uint8Array, E>, options?: Options | undefined) => HttpServerResponse;
/**
 * Creates a streamed file response for a file system path.
 *
 * **Details**
 *
 * The effect requires `HttpPlatform`, can fail with a platform error, and supports
 * options for status, headers, content type, offset, and byte range.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const file: (path: string, options?: (Options.WithContentType & {
    readonly bytesToRead?: ByteSize.Input | undefined;
    readonly chunkSize?: number | undefined;
    readonly offset?: ByteSize.Input | undefined;
}) | undefined) => Effect.Effect<HttpServerResponse, PlatformError, HttpPlatform>;
/**
 * Creates a streamed file response for a Web `File`-like value.
 *
 * **Details**
 *
 * The effect requires `HttpPlatform` and supports options for status, headers,
 * content type, offset, and byte range.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fileWeb: (file: Body.HttpBody.FileLike, options?: (Options.WithContentType & {
    readonly bytesToRead?: number | undefined;
    readonly chunkSize?: number | undefined;
    readonly offset?: number | undefined;
}) | undefined) => Effect.Effect<HttpServerResponse, never, HttpPlatform>;
/**
 * Returns a response with the specified header set to the supplied value.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setHeader: {
    /**
     * Returns a response with the specified header set to the supplied value.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string, value: string): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Returns a response with the specified header set to the supplied value.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, key: string, value: string): HttpServerResponse;
};
/**
 * Returns a response with the specified header removed.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const removeHeader: {
    /**
     * Returns a response with the specified header removed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Returns a response with the specified header removed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, key: string): HttpServerResponse;
};
/**
 * Returns a response with all supplied headers set on the existing header map.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setHeaders: {
    /**
     * Returns a response with all supplied headers set on the existing header map.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (input: Headers.Input): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Returns a response with all supplied headers set on the existing header map.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, input: Headers.Input): HttpServerResponse;
};
/**
 * Returns a response with the cookie of the specified name removed.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const removeCookie: {
    /**
     * Returns a response with the cookie of the specified name removed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Returns a response with the cookie of the specified name removed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, name: string): HttpServerResponse;
};
/**
 * Returns a response with its cookie collection replaced by the supplied cookies.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const replaceCookies: {
    /**
     * Returns a response with its cookie collection replaced by the supplied cookies.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookies: Cookies.Cookies): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Returns a response with its cookie collection replaced by the supplied cookies.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, cookies: Cookies.Cookies): HttpServerResponse;
};
/**
 * Sets a cookie on the response.
 *
 * **Details**
 *
 * The effect fails with `CookiesError` if the cookie name, value, or options are
 * invalid.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setCookie: {
    /**
     * Sets a cookie on the response.
     *
     * **Details**
     *
     * The effect fails with `CookiesError` if the cookie name, value, or options are
     * invalid.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, value: string, options?: Cookies.Cookie["options"]): (self: HttpServerResponse) => Effect.Effect<HttpServerResponse, Cookies.CookiesError>;
    /**
     * Sets a cookie on the response.
     *
     * **Details**
     *
     * The effect fails with `CookiesError` if the cookie name, value, or options are
     * invalid.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, name: string, value: string, options?: Cookies.Cookie["options"]): Effect.Effect<HttpServerResponse, Cookies.CookiesError>;
};
/**
 * Sets an expired cookie on an `HttpServerResponse`.
 *
 * **Details**
 *
 * Returns an effect because cookie encoding can fail. The original response is not
 * mutated; the effect succeeds with a response containing the updated cookie set.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const expireCookie: {
    /**
     * Sets an expired cookie on an `HttpServerResponse`.
     *
     * **Details**
     *
     * Returns an effect because cookie encoding can fail. The original response is not
     * mutated; the effect succeeds with a response containing the updated cookie set.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, options?: Omit<NonNullable<Cookies.Cookie["options"]>, "expires" | "maxAge">): (self: HttpServerResponse) => Effect.Effect<HttpServerResponse, Cookies.CookiesError>;
    /**
     * Sets an expired cookie on an `HttpServerResponse`.
     *
     * **Details**
     *
     * Returns an effect because cookie encoding can fail. The original response is not
     * mutated; the effect succeeds with a response containing the updated cookie set.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, name: string, options?: Omit<NonNullable<Cookies.Cookie["options"]>, "expires" | "maxAge">): Effect.Effect<HttpServerResponse, Cookies.CookiesError>;
};
/**
 * Sets a cookie on an `HttpServerResponse`, throwing if the cookie cannot be
 * encoded.
 *
 * **When to use**
 *
 * Use when you need to set one trusted cookie and want encoding failures to
 * throw instead of being represented as `CookiesError` failures.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setCookieUnsafe: {
    /**
     * Sets a cookie on an `HttpServerResponse`, throwing if the cookie cannot be
     * encoded.
     *
     * **When to use**
     *
     * Use when you need to set one trusted cookie and want encoding failures to
     * throw instead of being represented as `CookiesError` failures.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, value: string, options?: Cookies.Cookie["options"]): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Sets a cookie on an `HttpServerResponse`, throwing if the cookie cannot be
     * encoded.
     *
     * **When to use**
     *
     * Use when you need to set one trusted cookie and want encoding failures to
     * throw instead of being represented as `CookiesError` failures.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, name: string, value: string, options?: Cookies.Cookie["options"]): HttpServerResponse;
};
/**
 * Sets an expired cookie on an `HttpServerResponse`, throwing if the expiration cookie
 * cannot be encoded.
 *
 * **When to use**
 *
 * Use when you need to expire one trusted cookie and want encoding failures to
 * throw instead of being represented as `CookiesError` failures.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const expireCookieUnsafe: {
    /**
     * Sets an expired cookie on an `HttpServerResponse`, throwing if the expiration cookie
     * cannot be encoded.
     *
     * **When to use**
     *
     * Use when you need to expire one trusted cookie and want encoding failures to
     * throw instead of being represented as `CookiesError` failures.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, options?: Omit<NonNullable<Cookies.Cookie["options"]>, "expires" | "maxAge">): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Sets an expired cookie on an `HttpServerResponse`, throwing if the expiration cookie
     * cannot be encoded.
     *
     * **When to use**
     *
     * Use when you need to expire one trusted cookie and want encoding failures to
     * throw instead of being represented as `CookiesError` failures.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, name: string, options?: Omit<NonNullable<Cookies.Cookie["options"]>, "expires" | "maxAge">): HttpServerResponse;
};
/**
 * Updates the cookies attached to an `HttpServerResponse` using the supplied
 * function.
 *
 * **Details**
 *
 * The original response is not mutated; a new response is returned with the
 * callback result as its cookie collection.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const updateCookies: {
    /**
     * Updates the cookies attached to an `HttpServerResponse` using the supplied
     * function.
     *
     * **Details**
     *
     * The original response is not mutated; a new response is returned with the
     * callback result as its cookie collection.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (f: (cookies: Cookies.Cookies) => Cookies.Cookies): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Updates the cookies attached to an `HttpServerResponse` using the supplied
     * function.
     *
     * **Details**
     *
     * The original response is not mutated; a new response is returned with the
     * callback result as its cookie collection.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, f: (cookies: Cookies.Cookies) => Cookies.Cookies): HttpServerResponse;
};
/**
 * Merges additional cookies into the cookies attached to an
 * `HttpServerResponse`.
 *
 * **Details**
 *
 * The original response is not mutated; a new response is returned with the merged
 * cookie collection.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const mergeCookies: {
    /**
     * Merges additional cookies into the cookies attached to an
     * `HttpServerResponse`.
     *
     * **Details**
     *
     * The original response is not mutated; a new response is returned with the merged
     * cookie collection.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookies: Cookies.Cookies): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Merges additional cookies into the cookies attached to an
     * `HttpServerResponse`.
     *
     * **Details**
     *
     * The original response is not mutated; a new response is returned with the merged
     * cookie collection.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, cookies: Cookies.Cookies): HttpServerResponse;
};
/**
 * Sets multiple cookies on an `HttpServerResponse`.
 *
 * **Details**
 *
 * Each input entry contains a cookie name, value, and optional cookie options. The
 * returned effect fails with `CookiesError` if any cookie cannot be encoded.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setCookies: {
    /**
     * Sets multiple cookies on an `HttpServerResponse`.
     *
     * **Details**
     *
     * Each input entry contains a cookie name, value, and optional cookie options. The
     * returned effect fails with `CookiesError` if any cookie cannot be encoded.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookies: Iterable<readonly [
        name: string,
        value: string,
        options?: Cookies.Cookie["options"]
    ]>): (self: HttpServerResponse) => Effect.Effect<HttpServerResponse, Cookies.CookiesError, never>;
    /**
     * Sets multiple cookies on an `HttpServerResponse`.
     *
     * **Details**
     *
     * Each input entry contains a cookie name, value, and optional cookie options. The
     * returned effect fails with `CookiesError` if any cookie cannot be encoded.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, cookies: Iterable<readonly [
        name: string,
        value: string,
        options?: Cookies.Cookie["options"]
    ]>): Effect.Effect<HttpServerResponse, Cookies.CookiesError, never>;
};
/**
 * Sets multiple cookies on an `HttpServerResponse`, throwing if any cookie cannot
 * be encoded.
 *
 * **When to use**
 *
 * Use when you need to set multiple trusted cookies and want encoding failures
 * to throw instead of being represented as `CookiesError` failures.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setCookiesUnsafe: {
    /**
     * Sets multiple cookies on an `HttpServerResponse`, throwing if any cookie cannot
     * be encoded.
     *
     * **When to use**
     *
     * Use when you need to set multiple trusted cookies and want encoding failures
     * to throw instead of being represented as `CookiesError` failures.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookies: Iterable<readonly [
        name: string,
        value: string,
        options?: Cookies.Cookie["options"]
    ]>): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Sets multiple cookies on an `HttpServerResponse`, throwing if any cookie cannot
     * be encoded.
     *
     * **When to use**
     *
     * Use when you need to set multiple trusted cookies and want encoding failures
     * to throw instead of being represented as `CookiesError` failures.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, cookies: Iterable<readonly [
        name: string,
        value: string,
        options?: Cookies.Cookie["options"]
    ]>): HttpServerResponse;
};
/**
 * Replaces the body of an `HttpServerResponse`.
 *
 * **Details**
 *
 * When the body carries a content type or content length, the returned response
 * includes the corresponding headers.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setBody: {
    /**
     * Replaces the body of an `HttpServerResponse`.
     *
     * **Details**
     *
     * When the body carries a content type or content length, the returned response
     * includes the corresponding headers.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: Body.HttpBody): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Replaces the body of an `HttpServerResponse`.
     *
     * **Details**
     *
     * When the body carries a content type or content length, the returned response
     * includes the corresponding headers.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, body: Body.HttpBody): HttpServerResponse;
};
/**
 * Sets the HTTP status code of an `HttpServerResponse`.
 *
 * **Details**
 *
 * When `statusText` is omitted, the existing status text is preserved.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setStatus: {
    /**
     * Sets the HTTP status code of an `HttpServerResponse`.
     *
     * **Details**
     *
     * When `statusText` is omitted, the existing status text is preserved.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (status: number, statusText?: string | undefined): (self: HttpServerResponse) => HttpServerResponse;
    /**
     * Sets the HTTP status code of an `HttpServerResponse`.
     *
     * **Details**
     *
     * When `statusText` is omitted, the existing status text is preserved.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpServerResponse, status: number, statusText?: string | undefined): HttpServerResponse;
};
/**
 * Returns whether a response must omit its body because its status is 204, 205,
 * or 304, or `withoutBody` is true.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export declare const omitsBody: (response: HttpServerResponse, withoutBody?: boolean) => boolean;
/**
 * Converts an `HttpServerResponse` to a Web `Response`.
 *
 * **Details**
 *
 * Cookies are appended as `Set-Cookie` headers. Stream bodies are converted using
 * the supplied context.
 *
 * Bodies are omitted for statuses 204, 205, and 304, or when `withoutBody` is true
 * (for example, for HEAD responses). Omitted raw `ReadableStream` bodies are
 * cancelled without awaiting completion, and cancellation errors are ignored.
 *
 * A raw `Response` body is returned as-is after the outer headers are merged
 * into it: outer headers replace native ones, and cookies are appended. When
 * its body is omitted, the bodyless `Response` keeps those merged headers and
 * uses the outer status for 204, 205 and 304, or the raw `Response` status
 * otherwise.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toWeb: (response: HttpServerResponse, options?: {
    readonly withoutBody?: boolean | undefined;
    readonly context?: Context.Context<never> | undefined;
}) => Response;
/**
 * Wraps an `HttpServerResponse` as an `HttpClientResponse`, using the optional
 * request for metadata and decode errors. Without a request, `url` is empty.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toClientResponse: (response: HttpServerResponse, options?: {
    readonly request?: HttpClientRequest.HttpClientRequest | undefined;
}) => HttpClientResponse.HttpClientResponse;
/**
 * Converts an `HttpClientResponse` to an `HttpServerResponse`.
 *
 * **Details**
 *
 * The response body is streamed from the client response. `Set-Cookie` headers are
 * removed from the header map and represented in the response cookie collection.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const fromClientResponse: (response: HttpClientResponse.HttpClientResponse) => HttpServerResponse;
/**
 * Converts a Web `Response` to an `HttpServerResponse`.
 *
 * **Details**
 *
 * `Set-Cookie` headers are parsed into the response cookie collection and removed
 * from the header map. A present Web body is exposed as a stream body.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const fromWeb: (response: Response) => HttpServerResponse;
export {};
//# sourceMappingURL=HttpServerResponse.d.ts.map