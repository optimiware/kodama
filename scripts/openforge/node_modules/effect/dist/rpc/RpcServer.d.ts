import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as HttpRouter from "../http/HttpRouter.ts";
import * as HttpServerRequest from "../http/HttpServerRequest.ts";
import * as HttpServerResponse from "../http/HttpServerResponse.ts";
import * as Layer from "../Layer.ts";
import * as Option from "../Option.ts";
import * as Queue from "../Queue.ts";
import * as Scope from "../Scope.ts";
import * as SocketServer from "../socket/SocketServer.ts";
import { Stdio } from "../Stdio.ts";
import type { WorkerError } from "../workers/WorkerError.ts";
import * as WorkerRunner from "../workers/WorkerRunner.ts";
import * as Rpc from "./Rpc.ts";
import type * as RpcGroup from "./RpcGroup.ts";
import type { FromClient, FromClientEncoded, FromServer, FromServerEncoded } from "./RpcMessage.ts";
import * as RpcSerialization from "./RpcSerialization.ts";
/**
 * The decoded RPC server boundary, accepting client messages for a client id
 * and allowing that client to be disconnected.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface RpcServer<A extends Rpc.Any> {
    readonly write: (clientId: number, message: FromClient<A>, options?: {
        readonly onRequest?: (<A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>) | undefined;
    }) => Effect.Effect<void>;
    readonly disconnect: (clientId: number) => Effect.Effect<void>;
}
/**
 * Creates an RPC server for an already-decoded message channel, running
 * handlers for a group and sending decoded server responses through
 * `onFromServer`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeNoSerialization: <Rpcs extends Rpc.Any>(group: RpcGroup.RpcGroup<Rpcs>, options: {
    readonly onFromServer: (response: FromServer<Rpcs>) => Effect.Effect<void>;
    readonly disableTracing?: boolean | undefined;
    readonly disableSpanPropagation?: boolean | undefined;
    readonly spanPrefix?: string | undefined;
    readonly spanAttributes?: Record<string, unknown> | undefined;
    readonly disableClientAcks?: boolean | undefined;
    readonly concurrency?: number | "unbounded" | undefined;
    readonly disableFatalDefects?: boolean | undefined;
}) => Effect.Effect<RpcServer<Rpcs>, never, Rpc.ToHandler<Rpcs> | Rpc.Middleware<Rpcs> | Scope.Scope>;
/**
 * Runs an RPC server for a group using the current server `Protocol`, decoding
 * requests, invoking handlers, encoding responses, and managing in-flight
 * request lifetime.
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export declare const make: <Rpcs extends Rpc.Any>(group: RpcGroup.RpcGroup<Rpcs>, options?: {
    readonly disableTracing?: boolean | undefined;
    readonly spanPrefix?: string | undefined;
    readonly spanAttributes?: Record<string, unknown> | undefined;
    readonly concurrency?: number | "unbounded" | undefined;
    readonly disableFatalDefects?: boolean | undefined;
} | undefined) => Effect.Effect<never, never, Protocol | Rpc.ToHandler<Rpcs> | Rpc.Middleware<Rpcs> | Rpc.ServicesServer<Rpcs>>;
/**
 * Provides a scoped layer that starts an RPC server for a group using the
 * current server `Protocol`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: <Rpcs extends Rpc.Any>(group: RpcGroup.RpcGroup<Rpcs>, options?: {
    readonly disableTracing?: boolean | undefined;
    readonly spanPrefix?: string | undefined;
    readonly spanAttributes?: Record<string, unknown> | undefined;
    readonly concurrency?: number | "unbounded" | undefined;
    readonly disableFatalDefects?: boolean | undefined;
}) => Layer.Layer<never, never, Protocol | Rpc.ToHandler<Rpcs> | Rpc.Middleware<Rpcs> | Rpc.ServicesServer<Rpcs>>;
/**
 * Creates a RPC server that registers a HTTP route with a `HttpRouter`.
 *
 * **Details**
 *
 * Defaults to using websockets for communication, but can be configured to use
 * HTTP.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerHttp: <Rpcs extends Rpc.Any>(options: {
    readonly group: RpcGroup.RpcGroup<Rpcs>;
    readonly path: HttpRouter.PathInput;
    readonly protocol?: "http" | "websocket" | undefined;
    readonly disableTracing?: boolean | undefined;
    readonly spanPrefix?: string | undefined;
    readonly spanAttributes?: Record<string, unknown> | undefined;
    readonly concurrency?: number | "unbounded" | undefined;
    readonly disableFatalDefects?: boolean | undefined;
    readonly streamBufferSize?: number | "unbounded" | undefined;
}) => Layer.Layer<never, never, RpcSerialization.RpcSerialization | HttpRouter.HttpRouter | Rpc.ToHandler<Rpcs> | Rpc.Middleware<Rpcs> | Rpc.ServicesServer<Rpcs>>;
declare const Protocol_base: Context.ServiceClass<Protocol, "effect/rpc/RpcServer/Protocol", {
    readonly run: (f: (clientId: number, data: FromClientEncoded) => Effect.Effect<void>) => Effect.Effect<never>;
    readonly disconnects: Queue.Dequeue<number>;
    readonly send: (clientId: number, response: FromServerEncoded, transferables?: ReadonlyArray<globalThis.Transferable>) => Effect.Effect<void>;
    readonly end: (clientId: number) => Effect.Effect<void>;
    readonly clientIds: Effect.Effect<ReadonlySet<number>>;
    readonly initialMessage: Effect.Effect<Option.Option<unknown>>;
    readonly supportsAck: boolean;
    readonly supportsTransferables: boolean;
    readonly supportsSpanPropagation: boolean;
    readonly supportsNotifications: boolean;
    /**
     * Builds the codec that fills the `unknown` holes of the protocol messages,
     * re-passed from the `RpcSerialization` backing this transport.
     */
    readonly codecFor: RpcSerialization.CodecFor;
}>;
/**
 * Defines the service interface for an RPC server transport, responsible for receiving
 * encoded client messages, sending encoded responses, tracking clients, and
 * declaring transport capabilities.
 *
 * **When to use**
 *
 * Use to provide the transport boundary for RPC servers over HTTP, WebSocket,
 * workers, sockets, or custom protocols.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class Protocol extends Protocol_base {
    /**
     * Creates a server protocol service from the supplied RPC implementation.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static make: <EX, RX>(f: (write: (clientId: number, data: FromClientEncoded) => Effect.Effect<void>) => Effect.Effect<Omit<{
        readonly run: (f: (clientId: number, data: FromClientEncoded) => Effect.Effect<void>) => Effect.Effect<never>;
        readonly disconnects: Queue.Dequeue<number>;
        readonly send: (clientId: number, response: FromServerEncoded, transferables?: ReadonlyArray<globalThis.Transferable>) => Effect.Effect<void>;
        readonly end: (clientId: number) => Effect.Effect<void>;
        readonly clientIds: Effect.Effect<ReadonlySet<number>>;
        readonly initialMessage: Effect.Effect<Option.Option<unknown>>;
        readonly supportsAck: boolean;
        readonly supportsTransferables: boolean;
        readonly supportsSpanPropagation: boolean;
        readonly supportsNotifications: boolean;
        /**
         * Builds the codec that fills the `unknown` holes of the protocol messages,
         * re-passed from the `RpcSerialization` backing this transport.
         */
        readonly codecFor: RpcSerialization.CodecFor;
    }, "run">, EX, RX>) => Effect.Effect<{
        readonly run: (f: (clientId: number, data: FromClientEncoded) => Effect.Effect<void>) => Effect.Effect<never>;
        readonly disconnects: Queue.Dequeue<number>;
        readonly send: (clientId: number, response: FromServerEncoded, transferables?: ReadonlyArray<globalThis.Transferable>) => Effect.Effect<void>;
        readonly end: (clientId: number) => Effect.Effect<void>;
        readonly clientIds: Effect.Effect<ReadonlySet<number>>;
        readonly initialMessage: Effect.Effect<Option.Option<unknown>>;
        readonly supportsAck: boolean;
        readonly supportsTransferables: boolean;
        readonly supportsSpanPropagation: boolean;
        readonly supportsNotifications: boolean;
        /**
         * Builds the codec that fills the `unknown` holes of the protocol messages,
         * re-passed from the `RpcSerialization` backing this transport.
         */
        readonly codecFor: RpcSerialization.CodecFor;
    }, EX, RX>;
}
/**
 * Creates a server `Protocol` backed by the current `SocketServer`, accepting
 * socket connections and routing decoded RPC messages.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare const makeProtocolSocketServer: Effect.Effect<{
    readonly run: (f: (clientId: number, data: FromClientEncoded) => Effect.Effect<void>) => Effect.Effect<never>;
    readonly disconnects: Queue.Dequeue<number>;
    readonly send: (clientId: number, response: FromServerEncoded, transferables?: ReadonlyArray<globalThis.Transferable>) => Effect.Effect<void>;
    readonly end: (clientId: number) => Effect.Effect<void>;
    readonly clientIds: Effect.Effect<ReadonlySet<number>>;
    readonly initialMessage: Effect.Effect<Option.Option<unknown>>;
    readonly supportsAck: boolean;
    readonly supportsTransferables: boolean;
    readonly supportsSpanPropagation: boolean;
    readonly supportsNotifications: boolean;
    /**
     * Builds the codec that fills the `unknown` holes of the protocol messages,
     * re-passed from the `RpcSerialization` backing this transport.
     */
    readonly codecFor: RpcSerialization.CodecFor;
}, never, RpcSerialization.RpcSerialization | Scope.Scope | SocketServer.SocketServer>;
/**
 * RPC protocol that uses `SocketServer` for communication.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerProtocolSocketServer: Layer.Layer<Protocol, never, RpcSerialization.RpcSerialization | SocketServer.SocketServer>;
/**
 * Creates a websocket server `Protocol` together with an HTTP effect that
 * upgrades the current request to a websocket and attaches it to the protocol.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare const makeProtocolWithHttpEffectWebsocket: Effect.Effect<{
    readonly protocol: Protocol["Service"];
    readonly httpEffect: Effect.Effect<HttpServerResponse.HttpServerResponse, never, Scope.Scope | HttpServerRequest.HttpServerRequest>;
}, never, RpcSerialization.RpcSerialization>;
/**
 * Creates a websocket server `Protocol` and registers its upgrade handler as a
 * GET route on the current `HttpRouter`.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare const makeProtocolWebsocket: (options: {
    readonly path: HttpRouter.PathInput;
}) => Effect.Effect<Protocol["Service"], never, RpcSerialization.RpcSerialization | HttpRouter.HttpRouter>;
/**
 * RPC protocol that uses WebSockets for communication.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerProtocolWebsocket: (options: {
    readonly path: HttpRouter.PathInput;
}) => Layer.Layer<Protocol, never, RpcSerialization.RpcSerialization | HttpRouter.HttpRouter>;
/**
 * Creates an HTTP request/response server `Protocol` together with an HTTP
 * effect that decodes the current request and streams or returns encoded RPC
 * responses.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare const makeProtocolWithHttpEffect: (options?: {
    readonly streamBufferSize?: number | "unbounded" | undefined;
} | undefined) => Effect.Effect<{
    readonly protocol: Protocol["Service"];
    readonly httpEffect: Effect.Effect<HttpServerResponse.HttpServerResponse, never, Scope.Scope | HttpServerRequest.HttpServerRequest>;
}, never, RpcSerialization.RpcSerialization>;
/**
 * Creates an HTTP server `Protocol` and registers its request handler as a POST
 * route on the current `HttpRouter`.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare const makeProtocolHttp: (options: {
    readonly path: HttpRouter.PathInput;
    readonly streamBufferSize?: number | "unbounded" | undefined;
}) => Effect.Effect<Protocol["Service"], never, RpcSerialization.RpcSerialization | HttpRouter.HttpRouter>;
/**
 * Provides a server `Protocol` that uses HTTP POST requests for RPC
 * communication.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerProtocolHttp: (options: {
    readonly path: HttpRouter.PathInput;
    readonly streamBufferSize?: number | "unbounded" | undefined;
}) => Layer.Layer<Protocol, never, RpcSerialization.RpcSerialization | HttpRouter.HttpRouter>;
/**
 * Starts an RPC server for a group and returns the HTTP request/response effect
 * that serves the non-websocket HTTP RPC protocol.
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export declare const toHttpEffect: <Rpcs extends Rpc.Any>(group: RpcGroup.RpcGroup<Rpcs>, options?: {
    readonly disableTracing?: boolean | undefined;
    readonly spanPrefix?: string | undefined;
    readonly spanAttributes?: Record<string, unknown> | undefined;
    readonly disableFatalDefects?: boolean | undefined;
    readonly streamBufferSize?: number | "unbounded" | undefined;
} | undefined) => Effect.Effect<Effect.Effect<HttpServerResponse.HttpServerResponse, never, Scope.Scope | HttpServerRequest.HttpServerRequest>, never, Scope.Scope | RpcSerialization.RpcSerialization | Rpc.ToHandler<Rpcs> | Rpc.Middleware<Rpcs> | Rpc.ServicesServer<Rpcs>>;
/**
 * Starts an RPC server for a group and returns the HTTP effect that upgrades
 * requests to the websocket RPC protocol.
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export declare const toHttpEffectWebsocket: <Rpcs extends Rpc.Any>(group: RpcGroup.RpcGroup<Rpcs>, options?: {
    readonly disableTracing?: boolean | undefined;
    readonly spanPrefix?: string | undefined;
    readonly spanAttributes?: Record<string, unknown> | undefined;
    readonly disableFatalDefects?: boolean | undefined;
} | undefined) => Effect.Effect<Effect.Effect<HttpServerResponse.HttpServerResponse, never, Scope.Scope | HttpServerRequest.HttpServerRequest>, never, Scope.Scope | RpcSerialization.RpcSerialization | Rpc.ToHandler<Rpcs> | Rpc.Middleware<Rpcs> | Rpc.ServicesServer<Rpcs>>;
/**
 * Creates a server `Protocol` that reads RPC messages from `Stdio.stdin` and
 * writes encoded responses to `Stdio.stdout`.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare const makeProtocolStdio: Effect.Effect<{
    readonly run: (f: (clientId: number, data: FromClientEncoded) => Effect.Effect<void>) => Effect.Effect<never>;
    readonly disconnects: Queue.Dequeue<number>;
    readonly send: (clientId: number, response: FromServerEncoded, transferables?: ReadonlyArray<globalThis.Transferable>) => Effect.Effect<void>;
    readonly end: (clientId: number) => Effect.Effect<void>;
    readonly clientIds: Effect.Effect<ReadonlySet<number>>;
    readonly initialMessage: Effect.Effect<Option.Option<unknown>>;
    readonly supportsAck: boolean;
    readonly supportsTransferables: boolean;
    readonly supportsSpanPropagation: boolean;
    readonly supportsNotifications: boolean;
    /**
     * Builds the codec that fills the `unknown` holes of the protocol messages,
     * re-passed from the `RpcSerialization` backing this transport.
     */
    readonly codecFor: RpcSerialization.CodecFor;
}, never, RpcSerialization.RpcSerialization | Scope.Scope | Stdio>;
/**
 * Provides a server `Protocol` that reads RPC messages from `Stdio.stdin` and
 * writes encoded responses to `Stdio.stdout`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerProtocolStdio: Layer.Layer<Protocol, never, RpcSerialization.RpcSerialization | Stdio>;
/**
 * Creates a server `Protocol` backed by `WorkerRunnerPlatform`, routing worker
 * messages to the RPC server and server responses back to workers.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare const makeProtocolWorkerRunner: Effect.Effect<Protocol["Service"], WorkerError, WorkerRunner.WorkerRunnerPlatform | Scope.Scope>;
/**
 * Provides a server `Protocol` backed by the current `WorkerRunnerPlatform`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerProtocolWorkerRunner: Layer.Layer<Protocol, WorkerError, WorkerRunner.WorkerRunnerPlatform>;
export {};
//# sourceMappingURL=RpcServer.d.ts.map