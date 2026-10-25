/**
 * Describes HTTP request and response bodies before they reach a platform
 * adapter.
 *
 * `HttpBody` is the shared body representation used by the HTTP modules. Each
 * variant stores the payload together with metadata that can be known before
 * sending it, such as `contentType` and `contentLength`. This module includes
 * body constructors for common payload shapes, support for schema-encoded JSON
 * bodies, streaming and file-backed bodies, and the error type used when body
 * construction fails.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as ByteSize from "../ByteSize.ts";
import * as Effect from "../Effect.ts";
import * as FileSystem from "../FileSystem.ts";
import * as Inspectable from "../Inspectable.ts";
import * as PlatformError from "../PlatformError.ts";
import * as Schema from "../Schema.ts";
import type { ParseOptions } from "../SchemaAST.ts";
import type { Issue } from "../SchemaIssue.ts";
import type * as Stream_ from "../Stream.ts";
import * as UrlParams from "./UrlParams.ts";
declare const TypeId = "~effect/http/HttpBody";
/**
 * Returns `true` if the provided value is an `HttpBody`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isHttpBody: (u: unknown) => u is HttpBody;
/**
 * Represents an HTTP request body.
 *
 * **Details**
 *
 * Supported variants include empty bodies, raw bodies, byte arrays, `FormData`, and streams of bytes.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type HttpBody = Empty | Raw | Uint8Array | FormData | Stream;
/**
 * Namespace containing type-level members associated with `HttpBody`.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace HttpBody {
    /**
     * Common protocol implemented by all HTTP body variants.
     *
     * **Details**
     *
     * It carries the variant tag plus optional `contentType` and `contentLength` metadata.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    interface Proto extends Inspectable.Inspectable {
        readonly [TypeId]: typeof TypeId;
        readonly _tag: string;
        readonly contentType?: string | undefined;
        readonly contentLength?: number | undefined;
    }
    /**
     * Minimal Web `File`-like shape used by HTTP helpers that need file metadata.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    interface FileLike {
        readonly name: string;
        readonly lastModified: number;
        readonly size: number;
        readonly stream: () => unknown;
        readonly type: string;
    }
}
declare const HttpBodyErrorTypeId = "~effect/http/HttpBody/HttpBodyError";
declare const HttpBodyError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "HttpBodyError";
} & Readonly<A>;
/**
 * Error produced while constructing an HTTP body from JSON or schema-encoded input.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class HttpBodyError extends HttpBodyError_base<{
    readonly reason: ErrorReason;
    readonly cause?: unknown;
}> {
    /**
     * Marks this value as an HTTP body error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [HttpBodyErrorTypeId] = "~effect/http/HttpBody/HttpBodyError";
}
/**
 * Checks whether a value is an `HttpBodyError`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isHttpBodyError: (u: unknown) => u is HttpBodyError;
/**
 * Reason for an `HttpBodyError`.
 *
 * **Details**
 *
 * `JsonError` represents a `JSON.stringify` failure; `SchemaError` represents a schema encoding issue.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type ErrorReason = {
    readonly _tag: "JsonError";
} | {
    readonly _tag: "SchemaError";
    readonly issue: Issue;
};
declare abstract class Proto implements HttpBody.Proto {
    readonly [TypeId]: typeof TypeId;
    abstract readonly _tag: string;
    constructor();
    abstract toJSON(): unknown;
    [Inspectable.NodeInspectSymbol](): unknown;
    toString(): string;
}
/**
 * HTTP body variant representing the absence of request content.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class Empty extends Proto {
    readonly _tag = "Empty";
    toJSON(): unknown;
}
/**
 * Provides the singleton empty HTTP body.
 *
 * **When to use**
 *
 * Use when you need an HTTP body value that represents no body content.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const empty: Empty;
/**
 * HTTP body variant containing an arbitrary runtime body value with optional content metadata.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class Raw extends Proto {
    readonly _tag = "Raw";
    readonly body: unknown;
    readonly contentType: string | undefined;
    readonly contentLength: number | undefined;
    constructor(body: unknown, contentType: string | undefined, contentLength: number | undefined);
    toJSON(): unknown;
}
/**
 * Creates a raw HTTP body from an arbitrary value and optional `contentType` and `contentLength` metadata.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const raw: (body: unknown, options?: {
    readonly contentType?: string | undefined;
    readonly contentLength?: number | undefined;
} | undefined) => Raw;
/**
 * HTTP body variant backed by a `Uint8Array`.
 *
 * **Details**
 *
 * It stores the bytes, content type, and byte length.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class Uint8Array extends Proto {
    readonly _tag = "Uint8Array";
    readonly contentType: string;
    readonly contentLength: number;
    /** Original text retained for adapters that can skip encoding. */
    readonly text: string | undefined;
    private _body;
    constructor(body: globalThis.Uint8Array | undefined, contentType: string, contentLength: number, text?: string);
    get body(): globalThis.Uint8Array;
    toJSON(): unknown;
}
/**
 * Creates a byte-array HTTP body.
 *
 * **Details**
 *
 * The content type defaults to `application/octet-stream`, and the content length is the byte array length.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const uint8Array: (body: globalThis.Uint8Array, contentType?: string) => Uint8Array;
/**
 * Creates a UTF-8 encoded text HTTP body.
 *
 * **Details**
 *
 * The content type defaults to `text/plain`. Text bodies are encoded lazily.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const text: (body: string, contentType?: string) => Uint8Array;
/**
 * Creates a JSON HTTP body using `JSON.stringify`, throwing if serialization fails.
 *
 * **Details**
 *
 * The content type defaults to `application/json`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const jsonUnsafe: (body: unknown, contentType?: string) => Uint8Array;
/**
 * Creates a JSON HTTP body in an `Effect`.
 *
 * **Details**
 *
 * `JSON.stringify` failures are captured as `HttpBodyError` values, and the content type defaults to `application/json`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const json: (body: unknown, contentType?: string) => Effect.Effect<Uint8Array, HttpBodyError>;
/**
 * Creates a JSON body constructor that first encodes values with the schema's JSON codec.
 *
 * **Details**
 *
 * Schema encoding issues and JSON serialization failures are returned as `HttpBodyError` values.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const jsonSchema: <S extends Schema.Constraint>(schema: S, options?: ParseOptions | undefined) => (body: S["Type"], contentType?: string) => Effect.Effect<Uint8Array, HttpBodyError, S["EncodingServices"]>;
/**
 * Creates an `application/x-www-form-urlencoded` HTTP body from `UrlParams`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const urlParams: (urlParams: UrlParams.Input, contentType?: string) => Uint8Array;
/**
 * HTTP body variant backed by Web `FormData`.
 *
 * **Details**
 *
 * The content type and content length are left unset so the runtime can supply multipart boundaries.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class FormData extends Proto {
    readonly _tag = "FormData";
    readonly contentType: undefined;
    readonly contentLength: undefined;
    readonly formData: globalThis.FormData;
    constructor(formData: globalThis.FormData);
    toJSON(): unknown;
}
/**
 * Wraps a Web `FormData` value as an HTTP body.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const formData: (body: globalThis.FormData) => FormData;
/**
 * Record input accepted by `formDataRecord`.
 *
 * **Details**
 *
 * Each field may be a single coercible value or an array of coercible values.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type FormDataInput = Record<string, FormDataCoercible | ReadonlyArray<FormDataCoercible>>;
/**
 * Value that can be appended by `formDataRecord`.
 *
 * **Details**
 *
 * `File` and `Blob` values are appended directly, primitive values are converted to strings, and `null` or `undefined` values are skipped.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type FormDataCoercible = string | number | boolean | globalThis.File | globalThis.Blob | null | undefined;
/**
 * Creates a `FormData` HTTP body from a record.
 *
 * **Details**
 *
 * Array fields append each item under the same key; primitive values are stringified, `File` and `Blob` values are appended directly, and nullish values are skipped.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const formDataRecord: (entries: FormDataInput) => FormData;
/**
 * HTTP body variant backed by a stream of `Uint8Array` chunks.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class Stream extends Proto {
    readonly _tag = "Stream";
    readonly stream: Stream_.Stream<globalThis.Uint8Array, unknown>;
    readonly contentType: string;
    readonly contentLength: number | undefined;
    constructor(stream: Stream_.Stream<globalThis.Uint8Array, unknown>, contentType: string, contentLength: number | undefined);
    toJSON(): unknown;
}
/**
 * Creates a streaming HTTP body from a stream of byte chunks.
 *
 * **Details**
 *
 * The content type defaults to `application/octet-stream`; content length is optional.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const stream: (body: Stream_.Stream<globalThis.Uint8Array, unknown>, contentType?: string, contentLength?: number) => Stream;
/**
 * Creates a streaming HTTP body for a file path.
 *
 * **Details**
 *
 * The effect requires `FileSystem`, stats the file to set the selected content length, and can fail with
 * `PlatformError`.
 *
 * Range validation and file access are deferred until the effect runs. Numeric range inputs must be
 * non-negative safe integers; bigint and byte-size strings can represent larger values. Malformed strings,
 * negative values, and non-finite, fractional, or unsafe numbers fail with `PlatformError` / `BadArgument`.
 *
 * The selected length is calculated exactly with bigint arithmetic and clamped to the bytes available after
 * `offset` (zero at or past EOF). Since `Stream.contentLength` is a number, a final length above
 * `Number.MAX_SAFE_INTEGER` fails with `BadArgument`. Larger sizes, offsets, and byte counts are valid when
 * the final clamped length is representable; the resulting Content-Length header preserves that exact length.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const file: (path: string, options?: {
    readonly bytesToRead?: ByteSize.Input | undefined;
    readonly chunkSize?: number | undefined;
    readonly offset?: ByteSize.Input | undefined;
    readonly contentType?: string | undefined;
}) => Effect.Effect<Stream, PlatformError.PlatformError, FileSystem.FileSystem>;
/**
 * Creates a streaming HTTP body for a file path using already-known file information.
 *
 * **Details**
 *
 * The effect requires `FileSystem`, uses the provided file size to determine the selected content length, and can
 * fail with `PlatformError`.
 *
 * Like {@link file}, this constructor validates ranges lazily and calculates the EOF-clamped length with exact
 * bigint arithmetic. Invalid range inputs and final lengths above `Number.MAX_SAFE_INTEGER` fail with
 * `PlatformError` / `BadArgument`. Larger sizes, offsets, and byte counts remain valid when the final length
 * is representable as a safe integer, preserving the exact Content-Length header.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fileFromInfo: (path: string, info: FileSystem.File.Info, options?: {
    readonly bytesToRead?: ByteSize.Input | undefined;
    readonly chunkSize?: number | undefined;
    readonly offset?: ByteSize.Input | undefined;
    readonly contentType?: string | undefined;
}) => Effect.Effect<Stream, PlatformError.PlatformError, FileSystem.FileSystem>;
export {};
//# sourceMappingURL=HttpBody.d.ts.map