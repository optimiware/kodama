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
import * as ByteSize from "../ByteSize.ts";
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as FileSystem from "../FileSystem.ts";
import * as Layer from "../Layer.ts";
import { type PlatformError } from "../PlatformError.ts";
import * as Etag from "./Etag.ts";
import * as Headers from "./Headers.ts";
import type * as Body from "./HttpBody.ts";
import * as Response from "./HttpServerResponse.ts";
declare const HttpPlatform_base: Context.ServiceClass<HttpPlatform, "effect/http/HttpPlatform", {
    readonly platform: "deno" | "node" | "bun" | "web";
    readonly compression: Compression;
    readonly fileResponse: (path: string, options?: Response.Options.WithContentType & {
        readonly bytesToRead?: ByteSize.Input | undefined;
        readonly chunkSize?: number | undefined;
        readonly offset?: ByteSize.Input | undefined;
    }) => Effect.Effect<Response.HttpServerResponse, PlatformError>;
    readonly fileWebResponse: (file: Body.HttpBody.FileLike, options?: Response.Options.WithContentType & {
        readonly bytesToRead?: number | undefined;
        readonly chunkSize?: number | undefined;
        readonly offset?: number | undefined;
    }) => Effect.Effect<Response.HttpServerResponse>;
}>;
/**
 * Service for platform-specific HTTP response helpers, including file-backed server responses.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class HttpPlatform extends HttpPlatform_base {
}
/**
 * Creates an `HttpPlatform` service from platform-specific file response constructors, using `FileSystem` and `Etag.Generator`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (impl: {
    readonly platform: "deno" | "node" | "bun" | "web";
    readonly compression: Compression;
    readonly fileResponse: (path: string, status: number, statusText: string | undefined, headers: Headers.Headers, start: number, end: number | undefined, contentLength: bigint) => Response.HttpServerResponse;
    readonly fileWebResponse: (file: Body.HttpBody.FileLike, status: number, statusText: string | undefined, headers: Headers.Headers, options?: {
        readonly bytesToRead?: number | undefined;
        readonly chunkSize?: number | undefined;
        readonly offset?: number | undefined;
    }) => Response.HttpServerResponse;
}) => Effect.Effect<HttpPlatform["Service"], never, Etag.Generator | FileSystem.FileSystem>;
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
export declare const layer: Layer.Layer<HttpPlatform, never, FileSystem.FileSystem>;
/**
 * Content codings that HTTP response compression can apply.
 *
 * @stability unstable
 * @category compression
 * @since 4.0.0
 */
export type CompressionAlgorithm = "gzip" | "deflate" | "br" | "zstd";
/**
 * Options passed to a platform when compressing a response body.
 *
 * **Details**
 *
 * The `level` scale depends on the algorithm. Platforms without a level knob,
 * such as the Web `CompressionStream` implementation, ignore it.
 *
 * @stability unstable
 * @category compression
 * @since 4.0.0
 */
export interface CompressionOptions {
    readonly level?: number | undefined;
}
/**
 * Platform primitive for HTTP response compression.
 *
 * **Details**
 *
 * `algorithms` advertises what the platform can encode; content negotiation
 * happens in the shared `HttpMiddleware.compression` middleware.
 *
 * `compressResponse` is only called when compression is definitely happening —
 * all skip logic runs in the shared middleware first. The platform owns the
 * body transform and removes `Content-Length` when the compressed size is not
 * known in advance. The `make` wrapper owns the `Content-Encoding` and `Vary`
 * headers.
 *
 * @stability unstable
 * @category compression
 * @since 4.0.0
 */
export interface Compression {
    readonly algorithms: ReadonlySet<CompressionAlgorithm>;
    readonly compressResponse: (response: Response.HttpServerResponse, algorithm: CompressionAlgorithm, options?: CompressionOptions | undefined) => Effect.Effect<Response.HttpServerResponse>;
}
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
export declare const compressionTransformWeb: (format: string) => (stream: ReadableStream<Uint8Array>) => ReadableStream<Uint8Array>;
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
export declare const makeCompressionWeb: (options: {
    readonly algorithms: Iterable<CompressionAlgorithm>;
    readonly transform: (algorithm: CompressionAlgorithm, options?: CompressionOptions | undefined) => (stream: ReadableStream<Uint8Array>) => ReadableStream<Uint8Array>;
}) => Compression;
export {};
//# sourceMappingURL=HttpPlatform.d.ts.map