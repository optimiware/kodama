/**
 * Models bidirectional socket connections in Effect.
 *
 * A `Socket` exposes a pull-based `reader` and a scoped `writer`. Acquiring
 * the reader dials the connection and returns an `Effect` that yields batches
 * of incoming frames with end-to-end backpressure: nothing is read from the
 * transport until the consumer pulls. Every termination, including clean
 * closes, surfaces as a `SocketError`, so reconnecting is a plain
 * `Effect.retry` around the scoped consume loop.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type { NonEmptyReadonlyArray } from "../Array.ts";
import type * as Cause from "../Cause.ts";
import * as Channel from "../Channel.ts";
import * as Context from "../Context.ts";
import type * as Duration from "../Duration.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import type * as Redacted from "../Redacted.ts";
import * as Schema from "../Schema.ts";
import * as Scope from "../Scope.ts";
import * as Stream from "../Stream.ts";
/**
 * Runtime type identifier attached to `Socket` services.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId = "~effect/socket/Socket";
/**
 * Returns `true` when a value is a `Socket`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isSocket: (u: unknown) => u is Socket;
/**
 * Service tag for bidirectional socket transports.
 *
 * **When to use**
 *
 * Use to access or provide the socket implementation used by programs that
 * read and write frames through the Effect environment.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const Socket: Context.Service<Socket, Socket>;
/**
 * Effect-based socket abstraction exposing a pull-based read side and a
 * scoped writer.
 *
 * **Details**
 *
 * Acquiring `reader` establishes the connection; the scope of the acquisition
 * owns the connection lifecycle. Its `pull` yields non-empty batches of incoming
 * frames and never completes via `Cause.Done`: every termination, clean close
 * included, fails with a `SocketError` wrapping the close reason. Code placed
 * between the acquisition and the first pull runs exactly once per
 * (re)connection, which makes handshakes plain code placement.
 *
 * Closing the acquisition scope must fail a pull that is currently suspended,
 * rather than leaving it blocked. Consumers such as `toChannel` rely on this
 * to shut a connection down without racing every pull against a separate
 * failure signal.
 *
 * The writer is detached from any single connection: acquiring it cannot
 * fail, and writes made while disconnected suspend until the next connection
 * is established. Releasing the writer scope half-closes the write side
 * where the transport supports it.
 *
 * **Example** (Consuming with automatic reconnect)
 *
 * ```ts skip-type-checking
 * Effect.gen(function*() {
 *   const { pull } = yield* socket.reader
 *   while (true) {
 *     yield* handle(yield* pull)
 *   }
 * }).pipe(
 *   Effect.scoped,
 *   Effect.retry({ schedule: Schedule.exponential(200) })
 * )
 * ```
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Socket {
    readonly [TypeId]: typeof TypeId;
    readonly reader: Effect.Effect<Reader, SocketError, Scope.Scope>;
    readonly writer: Effect.Effect<Writer, never, Scope.Scope>;
}
/**
 * The read side of a live `Socket` connection.
 *
 * **Details**
 *
 * `pull` reads the next non-empty batch. `upgrade` wraps this connection with
 * TLS when the transport supports it. Unsupported readers fail with a
 * `SocketUpgradeError`. The upgrade also fails with `SocketUpgradeError` when
 * the selected TLS role requires an identity but `key` and `cert` are not both
 * provided. Calling `upgrade()` without an options object uses the adapter's
 * client defaults.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Reader<A extends Uint8Array | string = Uint8Array | string> {
    readonly pull: Effect.Effect<NonEmptyReadonlyArray<A>, SocketError>;
    readonly upgrade: (options?: TlsUpgradeOptions) => Effect.Effect<void, SocketError>;
}
/**
 * The write side of a `Socket`.
 *
 * **Details**
 *
 * `write` sends a single frame or a `CloseEvent`; `writeAll` sends a batch of
 * frames, allowing transports to coalesce them into a single flush. Both
 * apply the transport's native backpressure before succeeding.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Writer {
    readonly write: (chunk: Uint8Array | string | CloseEvent) => Effect.Effect<void, SocketError>;
    readonly writeAll: (chunks: NonEmptyReadonlyArray<Uint8Array | string>) => Effect.Effect<void, SocketError>;
}
/**
 * TLS credentials and handshake settings used to upgrade a live socket.
 *
 * **Details**
 *
 * `key` and `cert` are optional for client upgrades that do not present a
 * client certificate. Server upgrades require both, and providing only one is
 * invalid. Missing or incomplete credentials fail the upgrade with a
 * `SocketUpgradeError` when the adapter needs an identity.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface TlsUpgradeOptions {
    readonly key?: Redacted.Redacted<string | Uint8Array> | ReadonlyArray<Redacted.Redacted<string | Uint8Array>> | undefined;
    readonly cert?: string | Uint8Array | ReadonlyArray<string | Uint8Array> | undefined;
    readonly ca?: string | Uint8Array | ReadonlyArray<string | Uint8Array> | undefined;
    readonly passphrase?: Redacted.Redacted<string> | undefined;
    readonly alpnProtocols?: ReadonlyArray<string> | undefined;
    readonly requestCert?: boolean | undefined;
    readonly rejectUnauthorized?: boolean | undefined;
}
/**
 * Constructs a `Socket` from a reader acquisition and a scoped writer.
 *
 * **Details**
 *
 * The reader must fail a suspended pull when its acquisition scope closes; see
 * `Socket` for why. A reader that leaves a pull blocked forever will hang any
 * consumer that shuts the socket down by closing that scope.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (options: {
    readonly reader: Socket["reader"];
    readonly writer: Socket["writer"];
}) => Socket;
/**
 * Acquires the socket's binary `pull`, encoding any string frames as UTF-8
 * bytes.
 *
 * **Details**
 *
 * When a pulled batch contains no string frames it is returned as-is, so
 * transports that only emit bytes (TCP) pay no per-chunk cost.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const readerBytes: (self: Socket) => Effect.Effect<Effect.Effect<NonEmptyReadonlyArray<Uint8Array>, SocketError>, SocketError, Scope.Scope>;
/**
 * Acquires the socket's string `pull`, decoding binary frames with the optional
 * text encoding.
 *
 * **Details**
 *
 * The `TextDecoder` is created once per acquisition.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const readerString: (self: Socket, encoding?: string | undefined) => Effect.Effect<Effect.Effect<NonEmptyReadonlyArray<string>, SocketError>, SocketError, Scope.Scope>;
declare const CloseEventTypeId = "~effect/socket/Socket/CloseEvent";
/**
 * Represents a socket close event value carrying a close code and optional
 * reason.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class CloseEvent {
    /**
     * Marks this value as a socket close event for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [CloseEventTypeId]: typeof CloseEventTypeId;
    readonly code: number;
    readonly reason?: string | undefined;
    constructor(code?: number, reason?: string);
    /**
     * Formats the close code and optional reason for display.
     *
     * @stability unstable
     * @since 4.0.0
     */
    toString(): string;
}
/**
 * Returns `true` when a value is a `CloseEvent`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isCloseEvent: (u: unknown) => u is CloseEvent;
/**
 * Type-level identifier used to mark `SocketError` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type SocketErrorTypeId = "~effect/socket/Socket/SocketError";
/**
 * Runtime type identifier attached to `SocketError` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const SocketErrorTypeId: SocketErrorTypeId;
/**
 * Returns `true` when a value is a `SocketError`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isSocketError: (u: unknown) => u is SocketError;
declare const SocketReadError_base: Schema.Class<SocketReadError, Schema.Struct<{
    readonly _tag: Schema.tag<"SocketReadError">;
    readonly cause: Schema.Defect;
}>, Cause.YieldableError>;
/**
 * Typed error for failures that occur while reading from a socket.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketReadError extends SocketReadError_base {
    /**
     * Default message used for socket read failures.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly message = "An error occurred during Read";
}
declare const SocketWriteError_base: Schema.Class<SocketWriteError, Schema.Struct<{
    readonly _tag: Schema.tag<"SocketWriteError">;
    readonly cause: Schema.Defect;
}>, Cause.YieldableError>;
/**
 * Typed error for failures that occur while writing to a socket.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketWriteError extends SocketWriteError_base {
    /**
     * Default message used for socket write failures.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly message = "An error occurred during Write";
}
declare const SocketOpenError_base: Schema.Class<SocketOpenError, Schema.Struct<{
    readonly _tag: Schema.tag<"SocketOpenError">;
    readonly kind: Schema.Literals<readonly ["Unknown", "Timeout"]>;
    readonly cause: Schema.Defect;
}>, Cause.YieldableError>;
/**
 * Typed error for failures that occur while opening a socket, including
 * unknown open failures and open timeouts.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketOpenError extends SocketOpenError_base {
    /**
     * Formats timeout and unknown open failures for display.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): "An error occurred during Open" | "timeout waiting for \"open\"";
}
declare const SocketUpgradeError_base: Schema.Class<SocketUpgradeError, Schema.Struct<{
    readonly _tag: Schema.tag<"SocketUpgradeError">;
    readonly cause: Schema.optional<Schema.Defect>;
}>, Cause.YieldableError>;
/**
 * Typed error for an unsupported or failed in-place TLS upgrade.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketUpgradeError extends SocketUpgradeError_base {
    /**
     * An upgrade implementation for transports that cannot wrap the connection
     * with TLS.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static readonly unsupported: Reader["upgrade"];
    get message(): "An error occurred during TLS upgrade" | "Socket does not support TLS upgrade";
}
declare const SocketCloseError_base: Schema.Class<SocketCloseError, Schema.Struct<{
    readonly _tag: Schema.tag<"SocketCloseError">;
    readonly code: Schema.Int;
    readonly closeReason: Schema.optional<Schema.String>;
}>, Cause.YieldableError>;
/**
 * Typed error for a socket close, carrying the close code and optional close
 * reason.
 *
 * **Details**
 *
 * Sockets never classify closes: any close, whatever the code, fails the
 * reader with a `SocketError` wrapping this reason. Consumers that treat a
 * close as normal catch it.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketCloseError extends SocketCloseError_base {
    get message(): string;
}
/**
 * Schema for all socket-specific error reasons.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare const SocketErrorReason: Schema.Union<readonly [typeof SocketReadError, typeof SocketWriteError, typeof SocketOpenError, typeof SocketUpgradeError, typeof SocketCloseError]>;
/**
 * Union of socket-specific read, write, open, upgrade, and close error reasons.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type SocketErrorReason = SocketReadError | SocketWriteError | SocketOpenError | SocketUpgradeError | SocketCloseError;
declare const SocketError_base: Schema.Class<SocketError, Schema.TaggedStruct<"SocketError", {
    readonly _tag: Schema.tag<"SocketError">;
    readonly reason: Schema.Union<readonly [typeof SocketReadError, typeof SocketWriteError, typeof SocketOpenError, typeof SocketUpgradeError, typeof SocketCloseError]>;
}>, Cause.YieldableError>;
/**
 * Tagged error that wraps socket read, write, open, upgrade, and close failures
 * while preserving the underlying reason.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketError extends SocketError_base {
    constructor(props: {
        readonly reason: SocketReadError | SocketWriteError | SocketOpenError | SocketUpgradeError | SocketCloseError;
    });
    /**
     * Marks this value as a socket error wrapper for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [SocketErrorTypeId]: SocketErrorTypeId;
    /**
     * Returns `true` when the value is a `SocketError`.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static is(u: unknown): u is SocketError;
    readonly message: string;
}
/**
 * Converts a `Socket` into a bidirectional binary `Channel`, encoding
 * incoming string frames as UTF-8 bytes and writing outgoing frame batches to
 * the socket.
 *
 * **Details**
 *
 * The read side is the socket's pull, so the channel is backpressured
 * end-to-end.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const toChannel: <IE = never>(self: Socket) => Channel.Channel<NonEmptyReadonlyArray<Uint8Array>, SocketError | IE, void, NonEmptyReadonlyArray<Uint8Array | string | CloseEvent>, IE>;
/**
 * Converts a `Socket` into a bidirectional string `Channel`, decoding binary
 * frames with the optional text encoding.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const toChannelString: {
    /**
     * Converts a `Socket` into a bidirectional string `Channel`, decoding binary
     * frames with the optional text encoding.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (encoding?: string | undefined): <IE>(self: Socket) => Channel.Channel<NonEmptyReadonlyArray<string>, SocketError | IE, void, NonEmptyReadonlyArray<Uint8Array | string | CloseEvent>, IE>;
    /**
     * Converts a `Socket` into a bidirectional string `Channel`, decoding binary
     * frames with the optional text encoding.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <IE>(self: Socket, encoding?: string | undefined): Channel.Channel<NonEmptyReadonlyArray<string>, SocketError | IE, void, NonEmptyReadonlyArray<Uint8Array | string | CloseEvent>, IE>;
};
/**
 * Creates a `Socket` to binary `Channel` adapter with a fixed upstream error
 * type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const toChannelWith: <IE = never>() => (self: Socket) => Channel.Channel<NonEmptyReadonlyArray<Uint8Array>, SocketError | IE, void, NonEmptyReadonlyArray<Uint8Array | string | CloseEvent>, IE>;
/**
 * Converts a `Socket` into a read-only binary `Stream` backed by the socket's
 * pull, so consumption is backpressured end-to-end.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const toStream: (self: Socket) => Stream.Stream<Uint8Array, SocketError>;
/**
 * Creates a binary socket `Channel` from the `Socket` service in the
 * environment.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeChannel: <IE = never>() => Channel.Channel<NonEmptyReadonlyArray<Uint8Array>, SocketError | IE, void, NonEmptyReadonlyArray<Uint8Array | string | CloseEvent>, IE, unknown, Socket>;
/**
 * Event payload exposed by a WebSocket implementation.
 *
 * **Details**
 *
 * The socket adapter only reads `data`, `code`, and `reason`; implementations
 * may expose additional fields.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface WebSocketEvent {
    readonly type?: string;
    readonly data?: unknown;
    readonly code?: number;
    readonly reason?: string;
}
/**
 * The subset of the WebSocket API required by `Socket`.
 *
 * **Details**
 *
 * This structural interface is intentionally independent of the DOM
 * `WebSocket` type. Node implementations such as `ws` expose the same
 * event-target methods, but are not assignable to `globalThis.WebSocket`
 * because their event payload types are runtime-specific.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface WebSocketLike {
    readonly readyState: number;
    addEventListener(type: "open" | "message" | "error" | "close", listener: (event: WebSocketEvent) => void, options?: {
        readonly once?: boolean;
    }): void;
    removeEventListener(type: "open" | "message" | "error" | "close", listener: (event: WebSocketEvent) => void): void;
    close(code?: number, reason?: string): void;
    send(data: string | Uint8Array<ArrayBuffer>): void;
}
/**
 * Common options understood by a WebSocket client implementation.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface WebSocketClientOptions {
    /**
     * Headers to include in the opening handshake.
     *
     * **Details**
     *
     * This is supported by Node and Bun WebSocket clients. Browser constructors
     * cannot set arbitrary handshake headers and must reject this option.
     */
    readonly headers?: Readonly<Record<string, string>> | undefined;
}
/**
 * Options accepted by a `WebSocketConstructor`.
 *
 * **Details**
 *
 * Browser-compatible constructors accept a protocol string or list. Node and
 * Bun constructors additionally accept `WebSocketClientOptions`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type WebSocketConstructorOptions = string | Array<string> | WebSocketClientOptions;
declare const WebSocket_base: Context.ServiceClass<WebSocket, "~effect/socket/Socket/WebSocket", WebSocketLike>;
/**
 * Context service for the active `WebSocket` instance.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class WebSocket extends WebSocket_base {
}
declare const WebSocketConstructor_base: Context.ServiceClass<WebSocketConstructor, "@effect/platform/Socket/WebSocketConstructor", (url: string, options?: WebSocketConstructorOptions | undefined) => WebSocketLike>;
/**
 * Context service for constructing `WebSocket` instances from a URL and
 * optional protocols or platform-specific options.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class WebSocketConstructor extends WebSocketConstructor_base {
}
/**
 * Layer that provides `WebSocketConstructor` using `globalThis.WebSocket`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerWebSocketConstructorGlobal: Layer.Layer<WebSocketConstructor>;
/**
 * Creates a `Socket` backed by a `WebSocketConstructor`, dialing the
 * WebSocket for each reader acquisition.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeWebSocket: (url: string | Effect.Effect<string>, options?: {
    readonly openTimeout?: Duration.Input | undefined;
    readonly protocols?: string | Array<string> | undefined;
    readonly highWaterMark?: number | undefined;
}) => Effect.Effect<Socket, never, WebSocketConstructor>;
/**
 * Builds a `Socket` from a scoped WebSocket acquisition effect.
 *
 * **Details**
 *
 * Reader acquisition runs `acquire`, attaches event listeners, then waits for
 * the socket to open. Implementations exposing `pause`/`resume` (the `ws`
 * package) remain flowing until buffered frames reach the configured
 * `highWaterMark` (64 KiB by default). Draining the buffer resumes the
 * transport. Incoming frames that arrive in the same tick are coalesced into
 * one batch. Implementations without pause (browsers) buffer incoming frames,
 * optionally failing the socket with a `SocketReadError` when `highWaterMark`
 * bytes are exceeded (default unbounded). Message boundaries survive: each
 * pulled batch contains one element per frame.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fromWebSocket: <RO, WS extends WebSocketLike>(acquire: Effect.Effect<WS, SocketError, RO>, options?: {
    readonly openTimeout?: Duration.Input | undefined;
    readonly highWaterMark?: number | undefined;
} | undefined) => Effect.Effect<Socket, never, Exclude<RO, Scope.Scope>>;
/**
 * Creates a binary `Channel` backed by a WebSocket URL, requiring a
 * `WebSocketConstructor` service.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeWebSocketChannel: <IE = never>(url: string, options?: {
    readonly openTimeout?: Duration.Input | undefined;
    readonly protocols?: string | Array<string> | undefined;
    readonly highWaterMark?: number | undefined;
}) => Channel.Channel<NonEmptyReadonlyArray<Uint8Array>, SocketError | IE, void, NonEmptyReadonlyArray<Uint8Array | string | CloseEvent>, IE, unknown, WebSocketConstructor>;
/**
 * Layer that provides a `Socket` service backed by a WebSocket URL or URL
 * effect.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerWebSocket: (url: string | Effect.Effect<string>, options?: {
    readonly openTimeout?: Duration.Input | undefined;
    readonly protocols?: string | Array<string> | undefined;
    readonly highWaterMark?: number | undefined;
} | undefined) => Layer.Layer<Socket, never, WebSocketConstructor>;
/**
 * Readable and writable stream pair used to adapt transform-style streams into
 * a `Socket`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface InputTransformStream {
    readonly readable: ReadableStream<Uint8Array> | ReadableStream<string> | ReadableStream<Uint8Array | string>;
    readonly writable: WritableStream<Uint8Array>;
}
/**
 * Builds a `Socket` from a scoped `InputTransformStream`, pulling incoming
 * chunks from the readable side and writing outgoing chunks to the writable
 * stream, encoding strings as UTF-8.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fromTransformStream: <R>(acquire: Effect.Effect<InputTransformStream, SocketError, R>) => Effect.Effect<Socket, never, Exclude<R, Scope.Scope>>;
export {};
//# sourceMappingURL=Socket.d.ts.map