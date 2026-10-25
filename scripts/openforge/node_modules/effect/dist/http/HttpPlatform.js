/**
 * Platform-specific support for serving files as HTTP server responses.
 *
 * `HttpPlatform` is the boundary between the portable HTTP response model and
 * the runtime that knows how to stream bytes from the host platform. Server
 * code uses this service when it needs to return local files, static assets,
 * downloads, byte ranges, or Web `File`-like values without constructing the
 * response body by hand.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as ByteSize from "../ByteSize.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as FileSystem from "../FileSystem.js";
import { identity } from "../Function.js";
import * as Layer from "../Layer.js";
import * as Option from "../Option.js";
import { badArgument } from "../PlatformError.js";
import * as Stream from "../Stream.js";
import * as Etag from "./Etag.js";
import * as Headers from "./Headers.js";
import * as Response from "./HttpServerResponse.js";
import * as internal from "./internal/compression.js";
import * as Mime from "./Mime.js";
/**
 * Service for platform-specific HTTP response helpers, including file-backed server responses.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class HttpPlatform extends /*#__PURE__*/Context.Service()("effect/http/HttpPlatform") {}
/**
 * Creates an `HttpPlatform` service from platform-specific file response constructors, using `FileSystem` and `Etag.Generator`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = /*#__PURE__*/Effect.fnUntraced(function* (impl) {
  const fs = yield* FileSystem.FileSystem;
  const etagGen = yield* Etag.Generator;
  return HttpPlatform.of({
    platform: impl.platform,
    compression: internal.wrapCompression(impl.compression),
    fileResponse: Effect.fnUntraced(function* (path, options) {
      const info = yield* fs.stat(path);
      const etag = yield* etagGen.fromFileInfo(info);
      const requestedOffset = options?.offset === undefined ? ByteSize.zero : yield* fileResponseSize(options.offset, "offset");
      const offset = requestedOffset > info.size ? info.size : requestedOffset;
      const available = info.size - offset;
      const bytesToRead = options?.bytesToRead !== undefined ? yield* fileResponseSize(options.bytesToRead, "bytesToRead") : undefined;
      const contentLength = bytesToRead === undefined || bytesToRead > available ? available : bytesToRead;
      const limit = bytesToRead === undefined ? undefined : offset + contentLength;
      const start = yield* fileResponseNumber(offset, "offset");
      const end = limit === undefined ? undefined : yield* fileResponseNumber(limit, "end");
      const headers = Headers.set(optionHeaders(options), "etag", Etag.toString(etag));
      if (Option.isSome(info.mtime)) {
        ;
        headers["last-modified"] = info.mtime.value.toUTCString();
      }
      return impl.fileResponse(path, options?.status ?? 200, options?.statusText, headers, start, end, contentLength);
    }),
    fileWebResponse(file, options) {
      return Effect.map(etagGen.fromFileWeb(file), etag => {
        const headers = Headers.merge(optionHeaders(options), Headers.fromRecordUnsafe({
          etag: Etag.toString(etag),
          "last-modified": new Date(file.lastModified).toUTCString()
        }));
        return impl.fileWebResponse(file, options?.status ?? 200, options?.statusText, headers, options);
      });
    }
  });
});
const optionHeaders = options => {
  const headers = options?.headers ? Headers.fromInput(options.headers) : Headers.empty;
  return options?.contentType ? Headers.set(headers, "content-type", options.contentType) : headers;
};
const fileResponseSize = (input, field) => {
  const size = ByteSize.fromInput(input);
  return Option.isSome(size) ? Effect.succeed(size.value) : Effect.fail(badArgument({
    module: "HttpPlatform",
    method: "fileResponse",
    description: `Invalid ${field}: ${input}`
  }));
};
const fileResponseNumber = (value, field) => {
  const number = Number(value);
  return Number.isSafeInteger(number) ? Effect.succeed(number) : Effect.fail(badArgument({
    module: "HttpPlatform",
    method: "fileResponse",
    description: `${field} exceeds the safe integer range: ${value}`
  }));
};
/**
 * Provides the default `HttpPlatform` implementation for serving file paths and
 * `File`-like values as streamed HTTP responses.
 *
 * **Details**
 *
 * The layer uses the `FileSystem` and weak ETag services to add file metadata
 * headers such as `etag` and `last-modified`. Missing content types are inferred
 * from the file extension.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layer = /*#__PURE__*/Layer.effect(HttpPlatform)(Effect.flatMap(FileSystem.FileSystem, fs => make({
  platform: "web",
  compression: internal.compressionWeb,
  fileResponse(path, status, statusText, headers, start, end, contentLength) {
    const length = Number(contentLength);
    return Response.stream(fs.stream(path, {
      offset: start,
      bytesToRead: end !== undefined ? end - start : undefined
    }), {
      contentType: headers["content-type"] ?? mimeType(path),
      // Omit unsafe numeric metadata so it cannot overwrite the exact header.
      contentLength: Number.isSafeInteger(length) ? length : undefined,
      headers: Headers.set(headers, "content-length", contentLength.toString()),
      status,
      statusText
    });
  },
  fileWebResponse(file, status, statusText, headers, options) {
    const offset = Math.min(Math.max(options?.offset ?? 0, 0), file.size);
    const available = file.size - offset;
    const contentLength = options?.bytesToRead === undefined ? available : Math.min(Math.max(options.bytesToRead, 0), available);
    const chunkSize = options?.chunkSize !== undefined ? Math.max(1, options.chunkSize) : Infinity;
    const end = offset + contentLength;
    const stream = end <= offset ? Stream.empty : Stream.fromReadableStream({
      evaluate: () => file.stream(),
      onError: identity
    }).pipe(Stream.mapAccum(() => 0, (position, bytes) => {
      const next = position + bytes.length;
      const start = Math.min(Math.max(offset - position, 0), bytes.length);
      const stop = Math.min(Math.max(end - position, 0), bytes.length);
      const chunks = [];
      for (let index = start; index < stop; index += chunkSize) {
        chunks.push({
          bytes: bytes.subarray(index, Math.min(index + chunkSize, stop)),
          done: next >= end && index + chunkSize >= stop
        });
      }
      return [next, chunks];
    }), Stream.takeUntil(chunk => chunk.done), Stream.map(chunk => chunk.bytes));
    return Response.stream(stream, {
      contentType: headers["content-type"] ?? (file.type === "" ? mimeType(file.name) : file.type),
      contentLength,
      headers,
      status,
      statusText
    });
  }
}))).pipe(/*#__PURE__*/Layer.provide(Etag.layerWeak));
const mimeType = path => Option.getOrElse(Mime.getType(path), () => "application/octet-stream");
/**
 * Creates a compression body transform backed by the Web `CompressionStream`
 * API, for use with `makeCompressionWeb`.
 *
 * **Details**
 *
 * The format string is passed through to the runtime, so runtime-specific
 * formats such as Bun's `"brotli"` and `"zstd"` are usable. `CompressionStream`
 * has no compression level knob, so `CompressionOptions.level` does not apply.
 *
 * @stability unstable
 * @category compression
 * @since 4.0.0
 */
export const compressionTransformWeb = internal.compressionTransformWeb;
/**
 * Creates a `Compression` implementation from Web `ReadableStream`
 * transforms.
 *
 * **Details**
 *
 * All supported bodies are transformed as streams. The `Content-Length`
 * header is dropped in every case.
 *
 * @stability unstable
 * @category compression
 * @since 4.0.0
 */
export const makeCompressionWeb = internal.makeCompressionWeb;
//# sourceMappingURL=HttpPlatform.js.map