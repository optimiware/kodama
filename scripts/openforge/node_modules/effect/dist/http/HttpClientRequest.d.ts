/**
 * Describes immutable outgoing HTTP client requests.
 *
 * `HttpClientRequest` is the request model shared by Effect HTTP clients and
 * platform adapters. A request stores its method, URL, query parameters, hash,
 * headers, and body as structured data. This module includes constructors,
 * helpers for updating requests, body encoders for common payloads, and
 * conversions to and from Web `Request` values.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type * as ByteSize from "../ByteSize.ts";
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import type * as FileSystem from "../FileSystem.ts";
import * as Inspectable from "../Inspectable.ts";
import * as Option from "../Option.ts";
import { type Pipeable } from "../Pipeable.ts";
import type * as PlatformError from "../PlatformError.ts";
import type * as Redacted from "../Redacted.ts";
import * as Result from "../Result.ts";
import type * as Schema from "../Schema.ts";
import type { ParseOptions } from "../SchemaAST.ts";
import * as Stream from "../Stream.ts";
import * as Headers from "./Headers.ts";
import * as HttpBody from "./HttpBody.ts";
import { type HttpMethod } from "./HttpMethod.ts";
import * as Url from "./Url.ts";
import * as UrlParams from "./UrlParams.ts";
declare const TypeId = "~effect/http/HttpClientRequest";
/**
 * Returns `true` when a value is an `HttpClientRequest`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isHttpClientRequest: (u: unknown) => u is HttpClientRequest;
/**
 * Immutable model of an outgoing HTTP client request, including its method, URL components, headers, and body.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface HttpClientRequest extends Inspectable.Inspectable, Pipeable {
    readonly [TypeId]: typeof TypeId;
    readonly method: HttpMethod;
    readonly url: string;
    readonly urlParams: UrlParams.UrlParams;
    readonly hash: Option.Option<string>;
    readonly headers: Headers.Headers;
    readonly body: HttpBody.HttpBody;
}
/**
 * Options for constructing or modifying an `HttpClientRequest`.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface Options {
    readonly method?: HttpMethod | undefined;
    readonly url?: string | URL | undefined;
    readonly urlParams?: UrlParams.Input | undefined;
    readonly hash?: string | undefined;
    readonly headers?: Headers.Input | undefined;
    readonly body?: HttpBody.HttpBody | undefined;
    readonly accept?: string | undefined;
    readonly acceptJson?: boolean | undefined;
}
/**
 * Namespace containing option types associated with `HttpClientRequest` construction.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Options {
    /**
     * Request options that omit the method and URL for helpers that already receive those values separately.
     *
     * @stability unstable
     * @category options
     * @since 4.0.0
     */
    interface NoUrl extends Omit<Options, "method" | "url"> {
    }
}
/**
 * Constructs an `HttpClientRequest` from fully normalized request components.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare function makeWith(method: HttpMethod, url: string, urlParams: UrlParams.Input, hash: Option.Option<string>, headers: Headers.Headers, body: HttpBody.HttpBody): HttpClientRequest;
/**
 * An empty `GET` request with no URL, query parameters, hash, headers, or body.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const empty: HttpClientRequest;
/**
 * Creates a request constructor for the specified HTTP method.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <M extends HttpMethod>(method: M) => (url: string | URL, options?: Options.NoUrl | undefined) => HttpClientRequest;
/**
 * Creates a `GET` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const get: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * Creates a `QUERY` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const query: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * Creates a `POST` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const post: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * Creates a `PATCH` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const patch: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * Creates a `PUT` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const put: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
declare const del: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * @stability unstable
 */
export { 
/**
 * Creates a `DELETE` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
del as delete };
/**
 * Creates a `HEAD` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const head: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * Creates an `OPTIONS` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const options: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * Creates a `TRACE` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const trace: (url: string | URL, options?: Options.NoUrl) => HttpClientRequest;
/**
 * Applies request options to an `HttpClientRequest`, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const modify: {
    /**
     * Applies request options to an `HttpClientRequest`, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (options: Options): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Applies request options to an `HttpClientRequest`, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, options: Options): HttpClientRequest;
};
/**
 * Sets the HTTP method on a request, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setMethod: {
    /**
     * Sets the HTTP method on a request, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (method: HttpMethod): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets the HTTP method on a request, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, method: HttpMethod): HttpClientRequest;
};
/**
 * Sets a single request header, replacing any existing value for that header.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setHeader: {
    /**
     * Sets a single request header, replacing any existing value for that header.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string, value: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets a single request header, replacing any existing value for that header.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, key: string, value: string): HttpClientRequest;
};
/**
 * Sets multiple request headers from an input collection, replacing existing values with matching names.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setHeaders: {
    /**
     * Sets multiple request headers from an input collection, replacing existing values with matching names.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (input: Headers.Input): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets multiple request headers from an input collection, replacing existing values with matching names.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, input: Headers.Input): HttpClientRequest;
};
/**
 * Transforms the request headers with the provided function, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const updateHeaders: {
    /**
     * Transforms the request headers with the provided function, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (f: (headers: Headers.Headers) => Headers.Headers): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Transforms the request headers with the provided function, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, f: (headers: Headers.Headers) => Headers.Headers): HttpClientRequest;
};
/**
 * Removes a single request header by name, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const removeHeader: {
    /**
     * Removes a single request header by name, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Removes a single request header by name, returning a new request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, key: string): HttpClientRequest;
};
/**
 * Sets the `Authorization` header using HTTP Basic authentication credentials.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const basicAuth: {
    /**
     * Sets the `Authorization` header using HTTP Basic authentication credentials.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (username: string | Redacted.Redacted, password: string | Redacted.Redacted): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets the `Authorization` header using HTTP Basic authentication credentials.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, username: string | Redacted.Redacted, password: string | Redacted.Redacted): HttpClientRequest;
};
/**
 * Sets the `Authorization` header using a bearer token.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bearerToken: {
    /**
     * Sets the `Authorization` header using a bearer token.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (token: string | Redacted.Redacted): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets the `Authorization` header using a bearer token.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, token: string | Redacted.Redacted): HttpClientRequest;
};
/**
 * Sets the `Accept` header to the specified media type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const accept: {
    /**
     * Sets the `Accept` header to the specified media type.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (mediaType: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets the `Accept` header to the specified media type.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, mediaType: string): HttpClientRequest;
};
/**
 * Sets the `Accept` header to `application/json`.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const acceptJson: (self: HttpClientRequest) => HttpClientRequest;
/**
 * Sets the request URL. When given a `URL`, its search parameters and hash are extracted into the request's structured fields.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setUrl: {
    /**
     * Sets the request URL. When given a `URL`, its search parameters and hash are extracted into the request's structured fields.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (url: string | URL): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets the request URL. When given a `URL`, its search parameters and hash are extracted into the request's structured fields.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, url: string | URL): HttpClientRequest;
};
/**
 * Prepends a URL segment to the request URL, inserting or trimming one slash as needed.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const prependUrl: {
    /**
     * Prepends a URL segment to the request URL, inserting or trimming one slash as needed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (path: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Prepends a URL segment to the request URL, inserting or trimming one slash as needed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, path: string): HttpClientRequest;
};
/**
 * Appends a URL segment to the request URL, inserting or trimming one slash as needed.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const appendUrl: {
    /**
     * Appends a URL segment to the request URL, inserting or trimming one slash as needed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (path: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Appends a URL segment to the request URL, inserting or trimming one slash as needed.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, path: string): HttpClientRequest;
};
/**
 * Updates the request URL by applying a function to the current URL string.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const updateUrl: {
    /**
     * Updates the request URL by applying a function to the current URL string.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (f: (url: string) => string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Updates the request URL by applying a function to the current URL string.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, f: (url: string) => string): HttpClientRequest;
};
/**
 * Sets one query parameter, replacing existing values for that parameter name.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setUrlParam: {
    /**
     * Sets one query parameter, replacing existing values for that parameter name.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string, value: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets one query parameter, replacing existing values for that parameter name.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, key: string, value: string): HttpClientRequest;
};
/**
 * Sets query parameters from an input collection, replacing existing values for matching names.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setUrlParams: {
    /**
     * Sets query parameters from an input collection, replacing existing values for matching names.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (input: UrlParams.Input): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets query parameters from an input collection, replacing existing values for matching names.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, input: UrlParams.Input): HttpClientRequest;
};
/**
 * Appends one query parameter value without removing existing values for the same name.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const appendUrlParam: {
    /**
     * Appends one query parameter value without removing existing values for the same name.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string, value: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Appends one query parameter value without removing existing values for the same name.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, key: string, value: string): HttpClientRequest;
};
/**
 * Appends query parameters from an input collection without removing existing values for matching names.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const appendUrlParams: {
    /**
     * Appends query parameters from an input collection without removing existing values for matching names.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (input: UrlParams.Input): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Appends query parameters from an input collection without removing existing values for matching names.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, input: UrlParams.Input): HttpClientRequest;
};
/**
 * Sets the URL fragment on a request without the leading `#`.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setHash: {
    /**
     * Sets the URL fragment on a request without the leading `#`.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (hash: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets the URL fragment on a request without the leading `#`.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, hash: string): HttpClientRequest;
};
/**
 * Removes the URL fragment from a request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const removeHash: (self: HttpClientRequest) => HttpClientRequest;
/**
 * Sets the request body and updates `Content-Type` and `Content-Length` headers from the body metadata when available.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setBody: {
    /**
     * Sets the request body and updates `Content-Type` and `Content-Length` headers from the body metadata when available.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: HttpBody.HttpBody): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets the request body and updates `Content-Type` and `Content-Length` headers from the body metadata when available.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, body: HttpBody.HttpBody): HttpClientRequest;
};
/**
 * Sets a `Uint8Array` request body with an optional content type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyUint8Array: {
    /**
     * Sets a `Uint8Array` request body with an optional content type.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: Uint8Array, contentType?: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets a `Uint8Array` request body with an optional content type.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, body: Uint8Array, contentType?: string): HttpClientRequest;
};
/**
 * Sets a text request body with an optional content type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyText: {
    /**
     * Sets a text request body with an optional content type.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: string, contentType?: string): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets a text request body with an optional content type.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, body: string, contentType?: string): HttpClientRequest;
};
/**
 * Encodes a value as a JSON request body and sets it on the request, failing with `HttpBodyError` if encoding fails.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyJson: {
    /**
     * Encodes a value as a JSON request body and sets it on the request, failing with `HttpBodyError` if encoding fails.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: unknown): (self: HttpClientRequest) => Effect.Effect<HttpClientRequest, HttpBody.HttpBodyError>;
    /**
     * Encodes a value as a JSON request body and sets it on the request, failing with `HttpBodyError` if encoding fails.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, body: unknown): Effect.Effect<HttpClientRequest, HttpBody.HttpBodyError>;
};
/**
 * Sets a JSON request body using unsafe JSON encoding.
 *
 * **When to use**
 *
 * Use when the request body is known to be JSON-serializable and a synchronous
 * `HttpClientRequest` result is needed.
 *
 * **Gotchas**
 *
 * JSON encoding may throw instead of failing in the Effect error channel.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyJsonUnsafe: {
    /**
     * Sets a JSON request body using unsafe JSON encoding.
     *
     * **When to use**
     *
     * Use when the request body is known to be JSON-serializable and a synchronous
     * `HttpClientRequest` result is needed.
     *
     * **Gotchas**
     *
     * JSON encoding may throw instead of failing in the Effect error channel.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: unknown): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets a JSON request body using unsafe JSON encoding.
     *
     * **When to use**
     *
     * Use when the request body is known to be JSON-serializable and a synchronous
     * `HttpClientRequest` result is needed.
     *
     * **Gotchas**
     *
     * JSON encoding may throw instead of failing in the Effect error channel.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, body: unknown): HttpClientRequest;
};
/**
 * Creates a schema-based JSON body encoder that sets the encoded value on a request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const schemaBodyJson: <S extends Schema.Constraint>(schema: S, options?: ParseOptions | undefined) => {
    (body: S["Type"]): (self: HttpClientRequest) => Effect.Effect<HttpClientRequest, HttpBody.HttpBodyError, S["EncodingServices"]>;
    (self: HttpClientRequest, body: S["Type"]): Effect.Effect<HttpClientRequest, HttpBody.HttpBodyError, S["EncodingServices"]>;
};
/**
 * Sets an `application/x-www-form-urlencoded` request body from URL parameter input.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyUrlParams: {
    /**
     * Sets an `application/x-www-form-urlencoded` request body from URL parameter input.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (input: UrlParams.Input): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets an `application/x-www-form-urlencoded` request body from URL parameter input.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, input: UrlParams.Input): HttpClientRequest;
};
/**
 * Sets a `FormData` request body.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyFormData: {
    /**
     * Sets a `FormData` request body.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: FormData): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets a `FormData` request body.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, body: FormData): HttpClientRequest;
};
/**
 * Creates a `FormData` request body from record-style entries and sets it on the request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyFormDataRecord: {
    /**
     * Creates a `FormData` request body from record-style entries and sets it on the request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (entries: HttpBody.FormDataInput): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Creates a `FormData` request body from record-style entries and sets it on the request.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, entries: HttpBody.FormDataInput): HttpClientRequest;
};
/**
 * Sets a streaming `Uint8Array` request body with optional content type and content length metadata.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyStream: {
    /**
     * Sets a streaming `Uint8Array` request body with optional content type and content length metadata.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (body: Stream.Stream<Uint8Array, unknown>, options?: {
        readonly contentType?: string | undefined;
        readonly contentLength?: number | undefined;
    } | undefined): (self: HttpClientRequest) => HttpClientRequest;
    /**
     * Sets a streaming `Uint8Array` request body with optional content type and content length metadata.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, body: Stream.Stream<Uint8Array, unknown>, options?: {
        readonly contentType?: string | undefined;
        readonly contentLength?: number | undefined;
    } | undefined): HttpClientRequest;
};
/**
 * Creates a file-backed request body from a filesystem path and sets it on the request.
 *
 * **Details**
 *
 * Uses {@link HttpBody.file} to validate ranges lazily and calculate the EOF-clamped content length with exact
 * bigint arithmetic. Invalid range inputs or a final length above `Number.MAX_SAFE_INTEGER` fail with
 * `PlatformError` / `BadArgument`. Larger sizes, offsets, and byte counts are valid when the final length is
 * representable as a safe integer. The request's Content-Length header contains that exact length.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const bodyFile: {
    /**
     * Creates a file-backed request body from a filesystem path and sets it on the request.
     *
     * **Details**
     *
     * Uses {@link HttpBody.file} to validate ranges lazily and calculate the EOF-clamped content length with exact
     * bigint arithmetic. Invalid range inputs or a final length above `Number.MAX_SAFE_INTEGER` fail with
     * `PlatformError` / `BadArgument`. Larger sizes, offsets, and byte counts are valid when the final length is
     * representable as a safe integer. The request's Content-Length header contains that exact length.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (path: string, options?: {
        readonly bytesToRead?: ByteSize.Input | undefined;
        readonly chunkSize?: number | undefined;
        readonly offset?: ByteSize.Input | undefined;
        readonly contentType?: string;
    }): (self: HttpClientRequest) => Effect.Effect<HttpClientRequest, PlatformError.PlatformError, FileSystem.FileSystem>;
    /**
     * Creates a file-backed request body from a filesystem path and sets it on the request.
     *
     * **Details**
     *
     * Uses {@link HttpBody.file} to validate ranges lazily and calculate the EOF-clamped content length with exact
     * bigint arithmetic. Invalid range inputs or a final length above `Number.MAX_SAFE_INTEGER` fail with
     * `PlatformError` / `BadArgument`. Larger sizes, offsets, and byte counts are valid when the final length is
     * representable as a safe integer. The request's Content-Length header contains that exact length.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: HttpClientRequest, path: string, options?: {
        readonly bytesToRead?: ByteSize.Input | undefined;
        readonly chunkSize?: number | undefined;
        readonly offset?: ByteSize.Input | undefined;
        readonly contentType?: string;
    }): Effect.Effect<HttpClientRequest, PlatformError.PlatformError, FileSystem.FileSystem>;
};
/**
 * Builds a `URL` from the request URL, query parameters, and hash, returning `Option.none()` if the URL is invalid.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare function toUrl(self: HttpClientRequest): Option.Option<URL>;
/**
 * Converts a Web `Request` into an `HttpClientRequest`, preserving method, URL, headers, and supported request bodies.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const fromWeb: (request: globalThis.Request) => HttpClientRequest;
/**
 * Converts an `HttpClientRequest` safely to a Web `Request` as a `Result`, failing when the request URL is invalid.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toWebResult: (self: HttpClientRequest, options?: {
    readonly signal?: AbortSignal | undefined;
    readonly context?: Context.Context<never> | undefined;
}) => Result.Result<Request, Url.UrlError>;
/**
 * Converts an `HttpClientRequest` to a Web `Request`, failing with `UrlError` when the request URL is invalid.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toWeb: (self: HttpClientRequest, options?: {
    readonly signal?: AbortSignal | undefined;
}) => Effect.Effect<Request, Url.UrlError>;
//# sourceMappingURL=HttpClientRequest.d.ts.map