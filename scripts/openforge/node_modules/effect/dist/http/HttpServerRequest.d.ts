/**
 * Provides server-side access to the current incoming HTTP request.
 *
 * `HttpServerRequest` is the context service used by handlers, middleware,
 * schema decoders, multipart parsers, WebSocket upgrades, and adapters. A
 * request stores its method, URL, original URL, headers, cookies, remote
 * address, body stream, and platform source object. This module also includes
 * request conversions and schema decoders for cookies, headers, search
 * parameters, JSON, forms, URL-encoded bodies, and multipart bodies.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type * as Arr from "../Array.ts";
import * as Channel from "../Channel.ts";
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import type * as FileSystem from "../FileSystem.ts";
import * as Option from "../Option.ts";
import type * as Path from "../Path.ts";
import type { ReadonlyRecord } from "../Record.ts";
import * as Result from "../Result.ts";
import * as Schema from "../Schema.ts";
import type { ParseOptions } from "../SchemaAST.ts";
import type * as Scope from "../Scope.ts";
import * as Socket from "../socket/Socket.ts";
import * as Stream from "../Stream.ts";
import * as Headers from "./Headers.ts";
import * as HttpClientRequest from "./HttpClientRequest.ts";
import * as HttpIncomingMessage from "./HttpIncomingMessage.ts";
import { type HttpMethod } from "./HttpMethod.ts";
import { HttpServerError, type RequestError } from "./HttpServerError.ts";
import * as Multipart from "./Multipart.ts";
/**
 * @stability unstable
 */
export { 
/**
 * Provides the `MaxBodySize` fiber reference for configuring request body limits.
 *
 * **When to use**
 *
 * Use to configure the maximum body size accepted while reading server
 * request bodies.
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
MaxBodySize } from "./HttpIncomingMessage.ts";
/**
 * Runtime type identifier for `HttpServerRequest` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId = "~effect/http/HttpServerRequest";
/**
 * Server-side representation of an incoming HTTP request.
 *
 * **Details**
 *
 * It extends `HttpIncomingMessage` with request metadata, parsed cookies,
 * multipart accessors, WebSocket upgrade support, and a `modify` method for
 * creating adjusted request views.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface HttpServerRequest extends HttpIncomingMessage.HttpIncomingMessage<HttpServerError> {
    readonly [TypeId]: typeof TypeId;
    readonly source: object;
    readonly url: string;
    readonly originalUrl: string;
    readonly method: HttpMethod;
    readonly cookies: ReadonlyRecord<string, string>;
    readonly multipart: Effect.Effect<Multipart.Persisted, Multipart.MultipartError, Scope.Scope | FileSystem.FileSystem | Path.Path>;
    readonly multipartStream: Stream.Stream<Multipart.Part, Multipart.MultipartError>;
    readonly upgrade: Effect.Effect<Socket.Socket, HttpServerError>;
    readonly modify: (options: {
        readonly url?: string;
        readonly headers?: Headers.Headers;
        readonly remoteAddress?: Option.Option<string>;
    }) => HttpServerRequest;
}
/**
 * Service tag for the active server-side HTTP request.
 *
 * **When to use**
 *
 * Use to access the request currently being handled by HTTP server routes and
 * middleware.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const HttpServerRequest: Context.Service<HttpServerRequest, HttpServerRequest>;
declare const ParsedSearchParams_base: Context.ServiceClass<ParsedSearchParams, "effect/http/ParsedSearchParams", ReadonlyRecord<string, string | string[]>>;
/**
 * Service that contains decoded URL query parameters for the current request.
 *
 * **When to use**
 *
 * Use to access query parameters that have already been parsed for the current
 * server request.
 *
 * **Details**
 *
 * Each key maps to a string value, or to an array when the parameter appears more
 * than once.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class ParsedSearchParams extends ParsedSearchParams_base {
}
/**
 * Converts a `URL` object's search parameters into a record.
 *
 * **Details**
 *
 * Repeated parameters are represented as arrays in insertion order.
 *
 * @stability unstable
 * @category parsing
 * @since 4.0.0
 */
export declare const searchParamsFromURL: (url: URL) => ReadonlyRecord<string, string | Array<string>>;
/**
 * Creates a channel backed by the current request's upgraded socket.
 *
 * **Details**
 *
 * The channel reads incoming socket messages and writes byte chunks to the
 * socket, failing if the request cannot be upgraded or the socket fails.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const upgradeChannel: <IE = never>() => Channel.Channel<Arr.NonEmptyReadonlyArray<Uint8Array>, HttpServerError | IE | Socket.SocketError, void, Arr.NonEmptyReadonlyArray<string | Uint8Array | Socket.CloseEvent>, IE, unknown, HttpServerRequest>;
/**
 * Decodes a schema from the cookies of the current request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaCookies: <A, I extends Readonly<Record<string, string | undefined>>, RD>(schema: Schema.ConstraintCodec<A, I, RD, unknown>, options?: ParseOptions | undefined) => Effect.Effect<A, Schema.SchemaError, RD | HttpServerRequest>;
/**
 * Decodes a schema from the headers of the current request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaHeaders: <A, I extends Readonly<Record<string, string | undefined>>, RD>(schema: Schema.ConstraintCodec<A, I, RD, unknown>, options?: ParseOptions | undefined) => Effect.Effect<A, Schema.SchemaError, HttpServerRequest | RD>;
/**
 * Decodes a schema from the parsed search parameters of the current request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaSearchParams: <A, I extends Readonly<Record<string, string | ReadonlyArray<string> | undefined>>, RD>(schema: Schema.ConstraintCodec<A, I, RD, unknown>, options?: ParseOptions | undefined) => Effect.Effect<A, Schema.SchemaError, ParsedSearchParams | RD>;
/**
 * Reads the current request body as JSON and decodes it with the supplied schema.
 *
 * **Details**
 *
 * The effect can fail if the body cannot be read or parsed, or if schema decoding
 * fails.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaBodyJson: <A, RD>(schema: Schema.ConstraintDecoder<A, RD>, options?: (ParseOptions & HttpIncomingMessage.JsonOptions) | undefined) => Effect.Effect<A, HttpServerError | Schema.SchemaError, HttpServerRequest | RD>;
/**
 * Decodes the current request body as form data.
 *
 * **Details**
 *
 * Multipart requests are persisted and decoded as multipart data; other form
 * requests are decoded from URL-encoded body parameters.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaBodyForm: <A, I extends Partial<Multipart.Persisted>, RD>(schema: Schema.ConstraintCodec<A, I, RD, unknown>, options?: ParseOptions | undefined) => Effect.Effect<A, HttpServerError | Multipart.MultipartError | Schema.SchemaError, RD | FileSystem.FileSystem | HttpServerRequest | Path.Path | Scope.Scope>;
/**
 * Reads the current request body as URL-encoded parameters and decodes them with
 * the supplied schema.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaBodyUrlParams: <A, I extends Readonly<Record<string, string | ReadonlyArray<string> | undefined>>, RD>(schema: Schema.ConstraintCodec<A, I, RD, unknown>, options?: ParseOptions | undefined) => Effect.Effect<A, HttpServerError | Schema.SchemaError, HttpServerRequest | RD>;
/**
 * Persists the current multipart request body and decodes it with the supplied
 * schema.
 *
 * **Details**
 *
 * The effect requires the services needed to persist multipart files, including a
 * scope, file system, and path service.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaBodyMultipart: <A, I extends Partial<Multipart.Persisted>, RD>(schema: Schema.ConstraintCodec<A, I, RD, unknown>, options?: ParseOptions | undefined) => Effect.Effect<A, Multipart.MultipartError | Schema.SchemaError, HttpServerRequest | Scope.Scope | FileSystem.FileSystem | Path.Path | RD>;
/**
 * Creates a decoder for a JSON value stored in a form field.
 *
 * **Details**
 *
 * For multipart requests, the named multipart field is decoded as JSON. For
 * URL-encoded requests, the named parameter is decoded as JSON and then decoded
 * with the supplied schema.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaBodyFormJson: <A, RD>(schema: Schema.ConstraintDecoder<A, RD>, options?: (ParseOptions & HttpIncomingMessage.JsonOptions) | undefined) => (field: string) => Effect.Effect<A, HttpServerError | Schema.SchemaError, RD | FileSystem.FileSystem | HttpServerRequest | Path.Path | Scope.Scope>;
/**
 * Creates an `HttpServerRequest` view of an `HttpClientRequest`.
 *
 * **Details**
 *
 * If the client request can be converted to an absolute URL, that URL is used as
 * the original URL.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const fromClientRequest: (request: HttpClientRequest.HttpClientRequest) => HttpServerRequest;
/**
 * Wraps a Web `Request` as an `HttpServerRequest`.
 *
 * **Details**
 *
 * The request's current URL is stored without the scheme and host, while the
 * original Web URL remains available as `originalUrl`.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const fromWeb: (request: globalThis.Request) => HttpServerRequest;
/**
 * Converts an `HttpServerRequest` into an `HttpClientRequest`.
 *
 * **Details**
 *
 * The converted request preserves the method, headers, body stream, and a URL
 * derived from the request when possible.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toClientRequest: (request: HttpServerRequest) => HttpClientRequest.HttpClientRequest;
/**
 * Attempts to construct an absolute `URL` for a server request safely.
 *
 * **Details**
 *
 * The host comes from the `host` header, defaulting to `localhost`, and the
 * protocol is `https` only when `x-forwarded-proto` is `https`; invalid URLs
 * return `Option.none`.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toURL: (self: HttpServerRequest) => Option.Option<URL>;
/**
 * Converts an `HttpServerRequest` safely to a Web `Request` as a `Result`.
 *
 * **Details**
 *
 * If the source is already a Web `Request`, it is returned unchanged. Otherwise
 * an absolute URL is derived from the request; invalid URLs fail with a
 * `RequestParseError`.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toWebResult: (self: HttpServerRequest, options?: {
    readonly signal?: AbortSignal | undefined;
    readonly context?: Context.Context<never> | undefined;
}) => Result.Result<Request, RequestError>;
/**
 * Converts an `HttpServerRequest` to a Web `Request` in `Effect`.
 *
 * **Details**
 *
 * The current context is used when streaming the request body into the Web
 * request.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toWeb: (self: HttpServerRequest, options?: {
    readonly signal?: AbortSignal | undefined;
}) => Effect.Effect<Request, RequestError>;
//# sourceMappingURL=HttpServerRequest.d.ts.map