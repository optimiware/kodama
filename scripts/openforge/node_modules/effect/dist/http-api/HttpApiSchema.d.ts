/**
 * Attaches HTTP API metadata to Effect Schema values.
 *
 * This module is the schema-side bridge for HttpApi endpoint builders,
 * generated clients, and OpenAPI support. It does not define routes or perform
 * IO. Instead, the helpers annotate schemas so the surrounding HTTP API tooling
 * can choose response status codes, content types, body codecs, multipart
 * handling, and no-body response behavior.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Sse from "../encoding/Sse.ts";
import { type LazyArg } from "../Function.ts";
import * as HttpStatus from "../http/HttpStatus.ts";
import type * as Multipart_ from "../http/Multipart.ts";
import * as Schema from "../Schema.ts";
import * as SchemaAST from "../SchemaAST.ts";
import * as Stream from "../Stream.ts";
declare module "../Schema.ts" {
    namespace Annotations {
        interface Augment {
            readonly httpApiStatus?: number | undefined;
        }
    }
}
/**
 * HTTP API body encoding metadata used by payloads and responses.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Encoding = PayloadEncoding | ResponseEncoding;
/**
 * HTTP API request payload encoding metadata.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type PayloadEncoding = {
    readonly _tag: "Multipart";
    readonly mode: "buffered" | "stream";
    readonly contentType: string;
    readonly limits?: Multipart_.withLimits.Options | undefined;
} | {
    readonly _tag: "Json" | "FormUrlEncoded" | "Uint8Array" | "Text";
    readonly contentType: string;
};
/**
 * HTTP API response body encoding metadata.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ResponseEncoding = {
    readonly _tag: "Json" | "FormUrlEncoded" | "Uint8Array" | "Text";
    readonly contentType: string;
};
declare const StreamSchemaTypeId = "~effect/http-api/HttpApiSchema/Stream";
/**
 * Common HTTP status code literals accepted by {@link status}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type StatusLiteral = HttpStatus.Literal;
/**
 * Sets the HTTP status code of a schema.
 *
 * **Details**
 *
 * This is equivalent to calling `.annotate({ httpApiStatus: code })` on the
 * schema. You can pass either a numeric status code (for example, `201`) or a
 * common literal name (for example, `"Created"`).
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare function status(code: number): {
    <S extends Schema.Top>(self: S): S["Rebuild"];
};
/**
 * @stability unstable
 */
export declare function status(code: StatusLiteral): {
    <S extends Schema.Top>(self: S): S["Rebuild"];
};
/**
 * Creates a void schema with the given HTTP status code.
 * This is used to represent empty responses with a specific status code.
 *
 * @see {@link NoContent} for the predefined 204 no content schema.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Empty: (code: number) => Schema.Void;
/**
 * Type of the `NoContent` schema, a void schema annotated with HTTP status code 204.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface NoContent extends Schema.Void {
}
/**
 * Schema for empty HTTP responses with status code 204.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const NoContent: NoContent;
/**
 * Type of the `Created` schema, a void schema annotated with HTTP status code 201.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Created extends Schema.Void {
}
/**
 * Schema for empty HTTP responses with status code 201.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const Created: Created;
/**
 * Type of the `Accepted` schema, a void schema annotated with HTTP status code 202.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Accepted extends Schema.Void {
}
/**
 * Schema for empty HTTP responses with status code 202.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const Accepted: Accepted;
/**
 * Schema type returned by `asNoContent`, encoding as `void` while decoding to the original schema type.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export interface asNoContent<S extends Schema.Constraint> extends Schema.decodeTo<Schema.toType<S>, Schema.Void> {
}
/**
 * Marks a schema as a no-content response while preserving a decoded client value.
 *
 * **Details**
 *
 * The server encodes the response as `void`; generated clients call `decode` to
 * produce the schema's decoded value when the response has no body.
 *
 * @see {@link NoContent} for a void schema with the status code 204.
 * @see {@link Empty} for creating a void schema with a specific status code.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function asNoContent<S extends Schema.Constraint>(options: {
    readonly decode: LazyArg<S["Type"]>;
}): (self: S) => asNoContent<S>;
/**
 * Mode describing whether an SSE stream emits full events or raw data values.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type StreamSseMode = "events" | "data";
/**
 * Schema for a Server-Sent Events success response.
 *
 * **Details**
 *
 * `events` describes successful application events emitted by the stream, and
 * `error` describes typed stream failures that will be encoded by later
 * endpoint/server/client integrations using the reserved failure event. If
 * `error` is omitted, it defaults to `Schema.Never`. When `StreamSse` is
 * constructed from `data`, handlers and clients expose raw data values while
 * the server and client still use an SSE event schema internally.
 *
 * **Gotchas**
 *
 * The client treats `effect/http-api/stream/failure` as a stream failure only
 * when its decoded `data` is a `Cause`. If an event schema accepts that name
 * dynamically but decodes `data` to another value, the client emits it as an
 * application event. Endpoint construction rejects event schemas that declare
 * the reserved name statically.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface StreamSse<Events extends Sse.EventCodec, Error extends Schema.Constraint, Value = Events["Type"]> extends Schema.BottomLazy<SchemaAST.Declaration, StreamSse<Events, Error, Value>> {
    readonly "Type": Stream.Stream<Value, Error["Type"], never>;
    readonly "Encoded": Stream.Stream<Value, Error["Type"], never>;
    readonly "DecodingServices": Events["DecodingServices"] | Error["DecodingServices"];
    readonly "EncodingServices": Events["EncodingServices"] | Error["EncodingServices"];
    readonly "Rebuild": StreamSse<Events, Error, Value>;
    readonly "~type.make.in": Stream.Stream<Value, Error["Type"], never>;
    readonly "~type.make": Stream.Stream<Value, Error["Type"], never>;
    readonly "Iso": Stream.Stream<Value, Error["Type"], never>;
    readonly [StreamSchemaTypeId]: typeof StreamSchemaTypeId;
    readonly _tag: "StreamSse";
    readonly mode: "sse";
    readonly sseMode: StreamSseMode;
    readonly contentType: string;
    readonly events: Events;
    readonly error: Error;
    readonly "~Value"?: Value | undefined;
}
/**
 * Event schema produced when {@link StreamSse} is constructed from a JSON data schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface SseEventFromData<Data extends Schema.Constraint> extends Schema.ConstraintCodec<{
    readonly id?: string | undefined;
    readonly event: string;
    readonly data: Data["Type"];
}, {
    readonly id?: string | undefined;
    readonly event?: string | undefined;
    readonly data: string;
}, Data["DecodingServices"], Data["EncodingServices"]> {
}
/**
 * Schema for a streaming `Uint8Array` success response.
 *
 * **Details**
 *
 * This declaration stores the response content type for later endpoint,
 * server, client, and OpenAPI integrations. It is intentionally separate from
 * the buffered `asUint8Array` response encoding.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface StreamUint8Array extends Schema.Bottom<Stream.Stream<Uint8Array, unknown, never>, Stream.Stream<Uint8Array, unknown, never>, never, never, SchemaAST.Declaration, StreamUint8Array> {
    readonly "Rebuild": StreamUint8Array;
    readonly [StreamSchemaTypeId]: typeof StreamSchemaTypeId;
    readonly _tag: "StreamUint8Array";
    readonly mode: "uint8array";
    readonly contentType: string;
}
/**
 * Schema for a streaming HTTP API success response.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type StreamSchema = StreamSse<Sse.EventCodec, Schema.Top, unknown> | StreamUint8Array;
/**
 * Creates a Server-Sent Events streaming success response schema.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const StreamSse: {
    /**
     * Creates a Server-Sent Events streaming success response schema.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <Events extends Sse.EventCodec, Error extends Schema.Constraint = Schema.Never>(options: {
        readonly contentType?: string | undefined;
        readonly events: Events;
        readonly error?: Error | undefined;
    }): StreamSse<Events, Error, Events["Type"]>;
    /**
     * Creates a Server-Sent Events streaming success response schema.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <Data extends Schema.Constraint, Error extends Schema.Constraint = Schema.Never>(options: {
        readonly contentType?: string | undefined;
        readonly data: Data;
        readonly error?: Error | undefined;
    }): StreamSse<SseEventFromData<Data>, Error, Data["Type"]>;
};
/**
 * Creates a streaming `Uint8Array` success response schema.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const StreamUint8Array: (options?: {
    readonly contentType?: string | undefined;
}) => StreamUint8Array;
/**
 * Runtime brand key used to mark `WithHeaders` response schemas.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const WithHeadersTypeId = "~effect/http-api/HttpApiSchema/WithHeaders";
/**
 * Type-level brand identifier used by `WithHeaders`.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type WithHeadersTypeId = typeof WithHeadersTypeId;
/**
 * Runtime brand key used to mark `WithHeaders` response values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const WithHeadersValueTypeId = "~effect/http-api/HttpApiSchema/WithHeadersValue";
/**
 * Type-level brand identifier used by `WithHeaders` response values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type WithHeadersValueTypeId = typeof WithHeadersValueTypeId;
/**
 * A response schema wrapping a body schema together with a response headers
 * schema.
 *
 * **Details**
 *
 * `WithHeaders` is a branded declaration schema: it carries the inner success
 * schema and the headers schema as properties, and server, client, and OpenAPI
 * integrations detect the brand and handle body and headers separately. It is
 * supported for error responses, though {@link encodeToWithHeaders} is usually
 * more convenient there because handlers can fail with the domain error value.
 *
 * - `schema` is the inner response schema. Success responses may wrap
 *   `StreamSse` and `StreamUint8Array`; error responses remain non-streaming.
 *   Nesting `WithHeaders` is rejected at construction.
 * - `headers` is any schema; endpoint construction applies
 *   `Schema.toCodecStringTree` unless codecs are disabled, so leaves become
 *   `string | undefined` on the wire and `undefined` leaves are omitted from the response.
 * - Status and response-encoding annotations are resolved from the wrapper
 *   first, falling through to the inner schema.
 * - A header-carrying response cannot share its status and content type with
 *   another response in the same success or error union. Endpoint construction
 *   rejects ambiguous declarations, including those made with
 *   {@link encodeToWithHeaders}.
 * - `Rebuild` preserves the brand and both parts, so `.annotate` keeps the
 *   wrapper intact.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface WithHeaders<S extends Schema.Top, H extends Schema.Top> extends Schema.Bottom<withHeaders<S["Type"], H["Type"]>, withHeaders<S["Encoded"], Schema.StringTree>, S["DecodingServices"] | H["DecodingServices"], S["EncodingServices"] | H["EncodingServices"], SchemaAST.Declaration, WithHeaders<S, H>> {
    readonly "Rebuild": WithHeaders<S, H>;
    readonly [WithHeadersTypeId]: typeof WithHeadersTypeId;
    readonly schema: S;
    readonly headers: H;
}
/**
 * The Type of a `WithHeaders` schema: what handlers return and what the
 * client resolves to, constructed via {@link withHeaders}.
 *
 * **Details**
 *
 * `body` is the inner success value. For stream success schemas it is the
 * `Stream` itself, so headers are decided before the body starts streaming.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface withHeaders<A, H> {
    readonly [WithHeadersValueTypeId]: WithHeadersValueTypeId;
    readonly body: A;
    readonly headers: H;
}
/**
 * Wraps a success schema with a response headers schema.
 *
 * **Details**
 *
 * Headers accept either a schema or a fields shorthand, mirroring the
 * request-side headers option.
 *
 * **Example** (Adding response headers to a success schema)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { HttpApiSchema } from "effect/http-api"
 *
 * const schema = HttpApiSchema.WithHeaders(Schema.String, {
 *   "x-total-count": Schema.FiniteFromString
 * })
 * const response: typeof schema.Type = HttpApiSchema.withHeaders({
 *   body: "created",
 *   headers: { "x-total-count": 1 }
 * })
 *
 * HttpApiSchema.isWithHeaders(schema) // => true
 * response.body // => "created"
 * response.headers // => { "x-total-count": 1 }
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare function WithHeaders<S extends Schema.Top, H extends Schema.Struct.Fields>(schema: S, headers: H): WithHeaders<S, Schema.Struct<H>>;
/**
 * @stability unstable
 */
export declare function WithHeaders<S extends Schema.Top, H extends Schema.Top>(schema: S, headers: H): WithHeaders<S, H>;
/**
 * Constructs a `WithHeaders` response value from a body and headers.
 *
 * **Details**
 *
 * The returned value is branded so servers and clients can detect it exactly,
 * including in mixed success unions. The same shape is used on both sides: a
 * value received from a client can be returned from another handler unchanged.
 *
 * See {@link WithHeaders} for an example that constructs a schema and its
 * corresponding response value.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const withHeaders: <A, H>(options: {
    readonly body: A;
    readonly headers: H;
}) => withHeaders<A, H>;
/**
 * Returns `true` when a schema is a `WithHeaders` response schema.
 *
 * **Example** (Detecting a response headers schema)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { HttpApiSchema } from "effect/http-api"
 *
 * const schema = HttpApiSchema.WithHeaders(Schema.String, {
 *   "x-request-id": Schema.String
 * })
 *
 * HttpApiSchema.isWithHeaders(schema) // => true
 * HttpApiSchema.isWithHeaders(Schema.String) // => false
 * ```
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export declare const isWithHeaders: (u: unknown) => u is WithHeaders<Schema.Top, Schema.Top>;
/**
 * Schema type returned by `encodeToWithHeaders`, encoding as a `{ body, headers }`
 * pair while decoding to the source schema type.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export interface encodeToWithHeaders<S extends Schema.Top, Body extends Schema.Top, Headers extends Schema.Struct.Fields> extends Schema.decodeTo<Schema.toType<S>, Schema.Struct<{
    readonly body: Body;
    readonly headers: Schema.Struct<Headers>;
}>> {
}
/**
 * Encodes a schema as a `{ body, headers }` pair, folding response headers into
 * an opaque domain type such as an error class.
 *
 * **Details**
 *
 * The encoded side is the pair of the body schema and the headers fields; the
 * Type stays the source schema's Type. The body schema is authoritative for
 * everything wire-level: status, content type, and response encoding resolve
 * from the body schema's annotations.
 *
 * The mappings are pure total functions: validation lives in the body and
 * header schemas, the mappings only reshape valid data.
 *
 * Streams used as the body schema turn mid-stream transport errors into
 * defects on the client. Stream responses should use {@link WithHeaders},
 * which preserves the body stream's error channel in the generated client.
 *
 * **Example** (Encoding an error with headers)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { HttpApiSchema } from "effect/http-api"
 *
 * class UserNotFound extends Schema.TaggedError<UserNotFound>()("UserNotFound", {
 *   userId: Schema.Int
 * }) {}
 *
 * const UserNotFoundWithHeaders = UserNotFound.pipe(
 *   HttpApiSchema.encodeToWithHeaders({
 *     body: HttpApiSchema.Empty(404),
 *     headers: {
 *       "x-user-id": Schema.Int
 *     }
 *   }, {
 *     decode: ({ headers }) => new UserNotFound({ userId: headers["x-user-id"] }),
 *     encode: (error) => ({
 *       headers: { "x-user-id": error.userId },
 *       body: undefined
 *     })
 *   })
 * )
 *
 * const encoded = Schema.encodeSync(UserNotFoundWithHeaders)(new UserNotFound({ userId: 123 }))
 * encoded // => { body: undefined, headers: { "x-user-id": 123 } }
 * ```
 *
 * @see {@link WithHeaders} for the structural wrapper recommended for success
 * responses, including streams.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function encodeToWithHeaders<S extends Schema.Top, Body extends Schema.Top, Headers extends Schema.Struct.Fields>(options: {
    readonly body: Body;
    readonly headers: Headers;
}, transformation: {
    readonly decode: (value: Schema.Struct.Type<{
        readonly body: Body;
        readonly headers: Schema.Struct<Headers>;
    }>) => S["Type"];
    readonly encode: (value: S["Type"]) => Schema.Struct.Type<{
        readonly body: Body;
        readonly headers: Schema.Struct<Headers>;
    }>;
}): (self: S) => encodeToWithHeaders<S, Body, Headers>;
/**
 * Runtime brand key used to mark schemas as buffered multipart payloads.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const MultipartTypeId = "~effect/http-api/HttpApiSchema/Multipart";
/**
 * Type-level brand identifier used by `asMultipart`.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type MultipartTypeId = typeof MultipartTypeId;
/**
 * Schema type returned by `asMultipart` for buffered multipart payloads.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export interface asMultipart<S extends Schema.Top> extends Schema.brand<S["Rebuild"], MultipartTypeId> {
}
/**
 * Marks a schema as a multipart payload.
 *
 * @see {@link asMultipartStream} for a multipart stream payload.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function asMultipart(options?: Multipart_.withLimits.Options): <S extends Schema.Top>(self: S) => asMultipart<S>;
/**
 * Runtime brand key used to mark schemas as streaming multipart payloads.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const MultipartStreamTypeId = "~effect/http-api/HttpApiSchema/MultipartStream";
/**
 * Type-level brand identifier used by `asMultipartStream`.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type MultipartStreamTypeId = typeof MultipartStreamTypeId;
/**
 * Schema type returned by `asMultipartStream` for streaming multipart payloads.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export interface asMultipartStream<S extends Schema.Top> extends Schema.brand<S["Rebuild"], MultipartStreamTypeId> {
}
/**
 * Marks a schema as a multipart stream payload.
 *
 * @see {@link asMultipart} for a buffered multipart payload.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function asMultipartStream(options?: Multipart_.withLimits.Options): <S extends Schema.Top>(self: S) => asMultipartStream<S>;
/**
 * Marks a schema as a JSON payload / response.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function asJson(options?: {
    readonly contentType?: string;
}): <S extends Schema.Top>(self: S) => S["Rebuild"];
/**
 * Marks a schema as an `application/x-www-form-urlencoded` payload or response.
 *
 * **Details**
 *
 * The schema's encoded side must be a record of strings.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function asFormUrlEncoded(options?: {
    readonly contentType?: string;
}): <S extends Schema.Top>(self: S) => S["Rebuild"];
/**
 * Marks a schema as a text payload / response.
 *
 * **Details**
 *
 * The schema encoded side must be a string.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function asText(options?: {
    readonly contentType?: string;
}): <S extends Schema.Top & {
    readonly Encoded: string;
}>(self: S) => S["Rebuild"];
/**
 * Marks a schema as a binary payload / response.
 *
 * **Details**
 *
 * The schema encoded side must be a `Uint8Array`.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function asUint8Array(options?: {
    readonly contentType?: string;
}): <S extends Schema.Top & {
    readonly Encoded: Uint8Array;
}>(self: S) => S["Rebuild"];
/**
 * Returns `true` when a schema AST represents a no-content response.
 *
 * **Details**
 *
 * The check succeeds for direct `void` schemas and schemas whose encoded or
 * transformation target is `void`.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export declare const isNoContent: (ast: SchemaAST.AST) => boolean;
export {};
//# sourceMappingURL=HttpApiSchema.d.ts.map