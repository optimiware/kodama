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
import * as ByteSize from "../ByteSize.js";
import * as Data from "../Data.js";
import * as Effect from "../Effect.js";
import * as FileSystem from "../FileSystem.js";
import { format } from "../Formatter.js";
import * as Inspectable from "../Inspectable.js";
import * as Option from "../Option.js";
import * as PlatformError from "../PlatformError.js";
import * as Predicate from "../Predicate.js";
import * as Schema from "../Schema.js";
import * as SchemaParser from "../SchemaParser.js";
import * as UrlParams from "./UrlParams.js";
const TypeId = "~effect/http/HttpBody";
/**
 * Returns `true` if the provided value is an `HttpBody`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpBody = u => Predicate.hasProperty(u, TypeId);
const HttpBodyErrorTypeId = "~effect/http/HttpBody/HttpBodyError";
/**
 * Error produced while constructing an HTTP body from JSON or schema-encoded input.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class HttpBodyError extends /*#__PURE__*/Data.TaggedError("HttpBodyError") {
  /**
   * Marks this value as an HTTP body error for runtime guards.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [HttpBodyErrorTypeId] = HttpBodyErrorTypeId;
}
/**
 * Checks whether a value is an `HttpBodyError`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpBodyError = u => Predicate.hasProperty(u, HttpBodyErrorTypeId);
class Proto {
  [TypeId];
  constructor() {
    this[TypeId] = TypeId;
  }
  [Inspectable.NodeInspectSymbol]() {
    return this.toJSON();
  }
  toString() {
    return format(this, {
      ignoreToString: true
    });
  }
}
/**
 * HTTP body variant representing the absence of request content.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class Empty extends Proto {
  _tag = "Empty";
  toJSON() {
    return {
      _id: "effect/HttpBody",
      _tag: "Empty"
    };
  }
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
export const empty = /*#__PURE__*/new Empty();
/**
 * HTTP body variant containing an arbitrary runtime body value with optional content metadata.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class Raw extends Proto {
  _tag = "Raw";
  body;
  contentType;
  contentLength;
  constructor(body, contentType, contentLength) {
    super();
    this.body = body;
    this.contentType = contentType;
    this.contentLength = contentLength;
  }
  toJSON() {
    return {
      _id: "effect/HttpBody",
      _tag: "Raw",
      body: this.body,
      contentType: this.contentType,
      contentLength: this.contentLength
    };
  }
}
/**
 * Creates a raw HTTP body from an arbitrary value and optional `contentType` and `contentLength` metadata.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const raw = (body, options) => new Raw(body, options?.contentType, options?.contentLength);
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
export class Uint8Array extends Proto {
  _tag = "Uint8Array";
  contentType;
  contentLength;
  /** Original text retained for adapters that can skip encoding. */
  text;
  _body;
  constructor(body, contentType, contentLength, text) {
    super();
    this._body = body;
    this.text = text;
    this.contentType = contentType;
    this.contentLength = contentLength;
  }
  get body() {
    return this._body ??= encodeText(this.text);
  }
  toJSON() {
    const toString = this.contentType.startsWith("text/") || this.contentType.endsWith("json");
    return {
      _id: "effect/HttpBody",
      _tag: "Uint8Array",
      body: toString ? new TextDecoder().decode(this.body) : `Uint8Array(${this.body.length})`,
      contentType: this.contentType,
      contentLength: this.contentLength
    };
  }
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
export const uint8Array = (body, contentType) => new Uint8Array(body, contentType ?? "application/octet-stream", body.length);
const encoder = /*#__PURE__*/new TextEncoder();
// Buffer encodes UTF-8 faster than TextEncoder when available.
const buffer = globalThis.Buffer;
const encodeText = buffer !== undefined ? body => buffer.from(body, "utf8") : body => encoder.encode(body);
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
export const text = (body, contentType) => {
  if (typeof body !== "string") {
    // Preserve untyped callers that relied on TextEncoder coercion.
    body = body === undefined ? "" : String(body);
  }
  if (buffer !== undefined) {
    return new Uint8Array(undefined, contentType ?? "text/plain", buffer.byteLength(body, "utf8"), body);
  }
  const bytes = encoder.encode(body);
  return new Uint8Array(bytes, contentType ?? "text/plain", bytes.length, body);
};
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
export const jsonUnsafe = (body, contentType) => text(JSON.stringify(body), contentType ?? "application/json");
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
export const json = (body, contentType) => Effect.try({
  try: () => text(JSON.stringify(body), contentType ?? "application/json"),
  catch: cause => new HttpBodyError({
    reason: {
      _tag: "JsonError"
    },
    cause
  })
});
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
export const jsonSchema = (schema, options) => {
  const encode = SchemaParser.encodeUnknownEffect(Schema.toCodecJson(schema));
  return (body, contentType) => encode(body, options).pipe(Effect.mapError(issue => new HttpBodyError({
    reason: {
      _tag: "SchemaError",
      issue
    },
    cause: issue
  })), Effect.flatMap(body => json(body, contentType)));
};
/**
 * Creates an `application/x-www-form-urlencoded` HTTP body from `UrlParams`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const urlParams = (urlParams, contentType) => text(UrlParams.toString(UrlParams.fromInput(urlParams)), contentType ?? "application/x-www-form-urlencoded");
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
export class FormData extends Proto {
  _tag = "FormData";
  contentType = undefined;
  contentLength = undefined;
  formData;
  constructor(formData) {
    super();
    this.formData = formData;
  }
  toJSON() {
    return {
      _id: "effect/HttpBody",
      _tag: "FormData",
      formData: this.formData
    };
  }
}
/**
 * Wraps a Web `FormData` value as an HTTP body.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const formData = body => new FormData(body);
const appendFormDataValue = (formData, key, value) => {
  if (value == null) {
    return;
  }
  if (typeof value === "object") {
    formData.append(key, value);
    return;
  }
  formData.append(key, String(value));
};
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
export const formDataRecord = entries => {
  const data = new globalThis.FormData();
  for (const [key, value] of Object.entries(entries)) {
    if (Array.isArray(value)) {
      for (const item of value) {
        appendFormDataValue(data, key, item);
      }
    } else {
      appendFormDataValue(data, key, value);
    }
  }
  return formData(data);
};
/**
 * HTTP body variant backed by a stream of `Uint8Array` chunks.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class Stream extends Proto {
  _tag = "Stream";
  stream;
  contentType;
  contentLength;
  constructor(stream, contentType, contentLength) {
    super();
    this.stream = stream;
    this.contentType = contentType;
    this.contentLength = contentLength;
  }
  toJSON() {
    return {
      _id: "effect/HttpBody",
      _tag: "Stream",
      contentType: this.contentType,
      contentLength: this.contentLength
    };
  }
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
export const stream = (body, contentType, contentLength) => new Stream(body, contentType ?? "application/octet-stream", contentLength);
const fileRangeSize = (input, field, method) => {
  const size = ByteSize.fromInput(input);
  return Option.isSome(size) ? Effect.succeed(size.value) : Effect.fail(PlatformError.badArgument({
    module: "HttpBody",
    method,
    description: `Invalid ${field}: ${input}`
  }));
};
const fileContentLength = /*#__PURE__*/Effect.fnUntraced(function* (size, method, options) {
  const offset = options?.offset === undefined ? ByteSize.zero : yield* fileRangeSize(options.offset, "offset", method);
  const bytesToRead = options?.bytesToRead === undefined ? undefined : yield* fileRangeSize(options.bytesToRead, "bytesToRead", method);
  const available = offset >= size ? BigInt("0") : size - offset;
  const length = bytesToRead === undefined || bytesToRead > available ? available : bytesToRead;
  if (length > BigInt(Number.MAX_SAFE_INTEGER)) {
    return yield* Effect.fail(PlatformError.badArgument({
      module: "HttpBody",
      method,
      description: `Content length exceeds Number.MAX_SAFE_INTEGER: ${length}`
    }));
  }
  return Number(length);
});
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
export const file = (path, options) => Effect.flatMap(FileSystem.FileSystem, fs => Effect.flatMap(fs.stat(path), info => Effect.map(fileContentLength(info.size, "file", options), contentLength => stream(fs.stream(path, options), options?.contentType, contentLength))));
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
export const fileFromInfo = (path, info, options) => Effect.flatMap(FileSystem.FileSystem, fs => Effect.map(fileContentLength(info.size, "fileFromInfo", options), contentLength => stream(fs.stream(path, options), options?.contentType, contentLength)));
//# sourceMappingURL=HttpBody.js.map