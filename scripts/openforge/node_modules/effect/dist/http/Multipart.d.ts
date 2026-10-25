/**
 * Parses and persists HTTP `multipart/form-data` request bodies.
 *
 * `Multipart` turns incoming byte streams into typed form parts. Text parts
 * become decoded fields, while upload parts stay as streamed files until they
 * are collected or written to scoped temporary files. The persisted
 * representation can then be decoded with schemas for handlers that receive
 * fields and uploaded files together. This module also includes multipart error
 * types, schema helpers for persisted files, and parser limit settings.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.ts";
import * as ByteSize from "../ByteSize.ts";
import * as Cause from "../Cause.ts";
import * as Channel from "../Channel.ts";
import * as Context from "../Context.ts";
import * as Data from "../Data.ts";
import * as Effect from "../Effect.ts";
import * as ErrorReporter from "../ErrorReporter.ts";
import * as FileSystem from "../FileSystem.ts";
import * as Inspectable from "../Inspectable.ts";
import * as Path from "../Path.ts";
import * as Schema from "../Schema.ts";
import type { ParseOptions } from "../SchemaAST.ts";
import type * as Scope from "../Scope.ts";
import * as Stream from "../Stream.ts";
import * as IncomingMessage from "./HttpIncomingMessage.ts";
import * as HttpServerRespondable from "./HttpServerRespondable.ts";
import * as HttpServerResponse from "./HttpServerResponse.ts";
import * as MP from "./MultipartParser.ts";
/**
 * Type identifier used to brand multipart part values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId = "~effect/http/Multipart";
/**
 * A parsed multipart part.
 *
 * **Details**
 *
 * A part is either a text `Field` or a streamed `File`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Part = Field | File;
/**
 * Namespace containing shared multipart part model types.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Part {
    /**
     * Common protocol implemented by multipart part values.
     *
     * **Details**
     *
     * It provides the multipart type identifier, tag, and inspectable behavior shared
     * by fields, files, and persisted files.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    interface Proto extends Inspectable.Inspectable {
        readonly [TypeId]: typeof TypeId;
        readonly _tag: string;
    }
}
/**
 * Multipart form field containing a decoded text value.
 *
 * **Details**
 *
 * The `key` is the field name, `contentType` is the part media type, and `value`
 * is the decoded field content.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Field extends Part.Proto {
    readonly _tag: "Field";
    readonly key: string;
    readonly contentType: string;
    readonly value: string;
}
/**
 * Returns `true` when a value has the multipart part type identifier.
 *
 * **Details**
 *
 * This includes `Field`, `File`, and `PersistedFile` values. Use
 * `isStreamPart` to identify only parsed, streamed parts.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isPart: (u: unknown) => u is Part;
/**
 * Returns `true` when a value is a multipart text `Field` or streamed `File`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isStreamPart: (u: unknown) => u is Part;
/**
 * Returns `true` when a value is a multipart text `Field`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isField: (u: unknown) => u is Field;
/**
 * Multipart file part.
 *
 * **Gotchas**
 *
 * The file content is exposed as a byte stream. `contentEffect` collects the full
 * file into memory and should be used only when the file size is acceptable.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface File extends Part.Proto {
    readonly _tag: "File";
    readonly key: string;
    readonly name: string;
    readonly contentType: string;
    readonly content: Stream.Stream<Uint8Array, MultipartError>;
    readonly contentEffect: Effect.Effect<Uint8Array, MultipartError>;
}
/**
 * Returns `true` when a value is a multipart `File`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isFile: (u: unknown) => u is File;
/**
 * Multipart file part that has been written to the filesystem.
 *
 * **Details**
 *
 * The `path` points to the persisted file while the scope used to persist the
 * multipart data remains open.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface PersistedFile extends Part.Proto {
    readonly _tag: "PersistedFile";
    readonly key: string;
    readonly name: string;
    readonly contentType: string;
    readonly path: string;
}
/**
 * Returns `true` when a value is a persisted multipart file.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isPersistedFile: (u: unknown) => u is PersistedFile;
/**
 * Record representation of persisted multipart data.
 *
 * **Details**
 *
 * Field names map to text values, arrays of text values, or arrays of
 * `PersistedFile` values.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Persisted {
    readonly [key: string]: ReadonlyArray<PersistedFile> | ReadonlyArray<string> | string;
}
declare const MultipartErrorTypeId = "~effect/http/Multipart/MultipartError";
/**
 * Error reason carried by a `MultipartError`.
 *
 * **Details**
 *
 * It identifies parser and limit failures such as oversized files or fields, too
 * many parts, total body size limits, parse errors, and internal errors.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class MultipartErrorReason extends Data.Error<{
    readonly _tag: "FileTooLarge" | "FieldTooLarge" | "BodyTooLarge" | "TooManyParts" | "InternalError" | "Parse";
    readonly cause?: unknown;
}> {
}
declare const MultipartError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => Cause.YieldableError & {
    readonly _tag: "MultipartError";
} & Readonly<A>;
/**
 * Error raised while parsing, streaming, or persisting multipart form data.
 *
 * **Details**
 *
 * The `reason` field contains the concrete `MultipartErrorReason`. When used as
 * a server response, parse errors render as `400`, limit errors as `413`, and
 * internal errors as `500`. Multipart errors are ignored by the error reporter.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class MultipartError extends MultipartError_base<{
    readonly reason: MultipartErrorReason;
}> implements HttpServerRespondable.Respondable {
    /**
     * Creates a multipart error from a reason tag and optional cause.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static fromReason(reason: MultipartErrorReason["_tag"], cause?: unknown): MultipartError;
    /**
     * Marks this value as a multipart error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [MultipartErrorTypeId] = "~effect/http/Multipart/MultipartError";
    readonly [ErrorReporter.ignore] = true;
    /**
     * Converts the multipart error into an HTTP response based on its reason.
     *
     * **Details**
     *
     * Parse errors produce `400`, size and part-count limits produce `413`, and
     * internal errors produce `500`.
     *
     * @stability unstable
     * @since 4.0.0
     */
    [HttpServerRespondable.symbol](): Effect.Effect<HttpServerResponse.HttpServerResponse, never, never>;
    /**
     * Uses the concrete multipart error reason as the public message.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
/**
 * Checks whether a value is a `MultipartError`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isMultipartError: (u: unknown) => u is MultipartError;
/**
 * Schema type for persisted multipart files.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export interface PersistedFileSchema extends Schema.declare<PersistedFile> {
}
/**
 * Schema for persisted multipart files.
 *
 * **Details**
 *
 * The encoded form contains the field key, original file name, content type, and
 * filesystem path.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const PersistedFileSchema: PersistedFileSchema;
/**
 * Schema for an array of persisted multipart files.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const FilesSchema: Schema.$Array<PersistedFileSchema>;
/**
 * Schema for exactly one persisted multipart file.
 *
 * **Details**
 *
 * The encoded form is a one-element file array, while the decoded value is the
 * single `PersistedFile`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const SingleFileSchema: Schema.decodeTo<PersistedFileSchema, Schema.$Array<PersistedFileSchema>>;
/**
 * Creates a decoder for persisted multipart data using the supplied schema.
 *
 * **Details**
 *
 * The returned function decodes an unknown input into the schema output and fails
 * with `SchemaError` when validation fails.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaPersisted: <A, I extends Partial<Persisted>, RD>(schema: Schema.ConstraintCodec<A, I, RD, unknown>) => (input: unknown, options?: ParseOptions) => Effect.Effect<A, Schema.SchemaError, RD>;
/**
 * Creates a decoder for a JSON-encoded field in persisted multipart data.
 *
 * **Details**
 *
 * The selected field is parsed from a JSON string and decoded with the supplied
 * schema.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const schemaJson: <A, RD>(schema: Schema.ConstraintDecoder<A, RD>, options?: (ParseOptions & IncomingMessage.JsonOptions) | undefined) => {
    (field: string): (persisted: Persisted) => Effect.Effect<A, Schema.SchemaError, RD>;
    (persisted: Persisted, field: string): Effect.Effect<A, Schema.SchemaError, RD>;
};
/**
 * Builds the low-level multipart parser configuration from request headers and
 * the current fiber context.
 *
 * **Details**
 *
 * Parser limits are read from the multipart references, including maximum parts,
 * field size, file size, total body size, and field MIME type overrides.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export declare const makeConfig: (headers: Record<string, string>) => Effect.Effect<MP.BaseConfig>;
/**
 * Creates a channel that parses multipart byte chunks into multipart parts.
 *
 * **Details**
 *
 * The channel consumes non-empty batches of `Uint8Array` chunks and emits
 * non-empty batches of parsed `Part` values, failing with `MultipartError` for
 * parser and limit failures.
 *
 * @stability unstable
 * @category parsing
 * @since 4.0.0
 */
export declare const makeChannel: <IE>(headers: Record<string, string>) => Channel.Channel<Arr.NonEmptyReadonlyArray<Part>, MultipartError | IE, void, Arr.NonEmptyReadonlyArray<Uint8Array>, IE, unknown>;
declare const defaultWriteFile: (path: string, file: File) => Effect.Effect<void, MultipartError, FileSystem.FileSystem>;
/**
 * Runs a channel of byte chunks and collects all output into a single
 * `Uint8Array`.
 *
 * **Gotchas**
 *
 * This materializes the full content in memory.
 * The source channel must not reuse or mutate emitted buffers, which are retained
 * until collection completes.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const collectUint8Array: <OE, OD, R>(self: Channel.Channel<Arr.NonEmptyReadonlyArray<Uint8Array>, OE, OD, unknown, unknown, unknown, R>) => Effect.Effect<Uint8Array<ArrayBuffer>, OE, R>;
/**
 * Persists a stream of multipart parts into a record.
 *
 * **Details**
 *
 * Text fields are collected as strings, and file parts are written to files in a
 * scoped temporary directory.
 *
 * **Gotchas**
 *
 * Persisted file paths remain valid for the lifetime of the scope.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toPersisted: (stream: Stream.Stream<Part, MultipartError>, writeFile?: typeof defaultWriteFile) => Effect.Effect<Persisted, MultipartError, FileSystem.FileSystem | Path.Path | Scope.Scope>;
/**
 * Creates a context containing multipart parser limit settings.
 *
 * **Details**
 *
 * The context can provide maximum part count, field size, file size, total body
 * size, and MIME types that should be parsed as fields.
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export declare const limitsServices: (options: {
    readonly maxParts?: number | undefined;
    readonly maxFieldSize?: ByteSize.Input | undefined;
    readonly maxFileSize?: ByteSize.Input | undefined;
    readonly maxTotalSize?: ByteSize.Input | undefined;
    readonly fieldMimeTypes?: ReadonlyArray<string> | undefined;
}) => Context.Context<never>;
/**
 * Namespace containing multipart parser limit option types.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace withLimits {
    /**
     * Options for overriding multipart parser limits.
     *
     * **Details**
     *
     * These settings control maximum part count, field size, file size, total body
     * size, and MIME types that should be treated as fields instead of files.
     *
     * @stability unstable
     * @category options
     * @since 4.0.0
     */
    type Options = {
        readonly maxParts?: number | undefined;
        readonly maxFieldSize?: ByteSize.Input | undefined;
        readonly maxFileSize?: ByteSize.Input | undefined;
        readonly maxTotalSize?: ByteSize.Input | undefined;
        readonly fieldMimeTypes?: ReadonlyArray<string> | undefined;
    };
}
/**
 * Context reference for the maximum number of multipart parts allowed.
 *
 * **Details**
 *
 * The default is `undefined`, meaning no explicit part-count limit.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const MaxParts: Context.Reference<number | undefined>;
/**
 * Context reference for the maximum size of a multipart field value.
 *
 * **Details**
 *
 * The default limit is 10 MiB.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const MaxFieldSize: Context.Reference<ByteSize.ByteSize>;
/**
 * Context reference for the maximum size of a multipart file part.
 *
 * **Details**
 *
 * The default is `undefined`, meaning no explicit per-file limit.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const MaxFileSize: Context.Reference<ByteSize.ByteSize | undefined>;
/**
 * Context reference for MIME type fragments that should be parsed as multipart
 * fields instead of files.
 *
 * **Details**
 *
 * The default treats `application/json` parts as fields.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const FieldMimeTypes: Context.Reference<readonly string[]>;
export {};
//# sourceMappingURL=Multipart.d.ts.map