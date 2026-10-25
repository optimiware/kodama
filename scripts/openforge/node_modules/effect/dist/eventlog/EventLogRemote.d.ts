import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import * as Queue from "../Queue.ts";
import * as RpcClient from "../rpc/RpcClient.ts";
import type { RpcClientError } from "../rpc/RpcClientError.ts";
import type * as Schema from "../Schema.ts";
import type * as Scope from "../Scope.ts";
import type { Entry, RemoteEntry, RemoteId } from "./EventJournal.ts";
import { type Identity, Registry } from "./EventLog.ts";
import { EventLogEncryption } from "./EventLogEncryption.ts";
import { ChunkedMessage, EventLogProtocolError, type HelloResponse, type StoreId } from "./EventLogMessage.ts";
declare const EventLogRemote_base: Context.ServiceClass<EventLogRemote, "effect/eventlog/EventLogRemote", {
    readonly id: RemoteId;
    readonly changes: (options: {
        readonly identity: Identity["Service"];
        readonly storeId: StoreId;
        readonly startSequence: number;
    }) => Effect.Effect<Queue.Dequeue<RemoteEntry, EventLogRemoteError>, never, Scope.Scope>;
    readonly write: (options: {
        readonly identity: Identity["Service"];
        readonly storeId: StoreId;
        readonly entries: ReadonlyArray<Entry>;
    }) => Effect.Effect<void, EventLogRemoteError>;
    readonly whenAuthenticated: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E | EventLogRemoteError, R | Identity>;
}>;
/**
 * Service that represents a remote event-log replica.
 *
 * **When to use**
 *
 * Use to access or provide a remote event-log replica that can write local
 * entries and stream remote changes.
 *
 * **Details**
 *
 * It can write local entries to the remote, stream remote changes from a sequence
 * number, and run effects only after the supplied identity has authenticated.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class EventLogRemote extends EventLogRemote_base {
}
declare const EventLogRemoteError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "EventLogRemoteError";
} & Readonly<A>;
/**
 * Error raised by `EventLogRemote` operations, recording the failed method and
 * underlying cause.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class EventLogRemoteError extends EventLogRemoteError_base<{
    readonly method: string;
    readonly cause: unknown;
}> {
    /**
     * Returns `true` when the value is an `EventLogRemoteError`.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static is(u: unknown): u is EventLogRemoteError;
}
declare const EventLogRemoteClient_base: Context.ServiceClass<EventLogRemoteClient, "effect/eventlog/EventLogRemote/EventLogRemoteClient", {
    readonly "EventLog.Authenticate": <const AsQueue extends boolean = false, const Discard = false>(input: {
        readonly publicKey: string;
        readonly signingPublicKey: Uint8Array<ArrayBuffer>;
        readonly signature: Uint8Array<ArrayBuffer>;
        readonly algorithm: "Ed25519";
    }, options?: {
        readonly headers?: import("../http/Headers.ts").Input | undefined;
        readonly context?: Context.Context<never> | undefined;
        readonly discard?: Discard | undefined;
    } | undefined) => Effect.Effect<Discard extends true ? void : void, RpcClientError | (Discard extends true ? never : EventLogProtocolError), never>;
    readonly "EventLog.Changes": <const AsQueue extends boolean = false, const Discard = false>(input: {
        readonly publicKey: string;
        readonly storeId: string & import("../Brand.ts").Brand<"effect/eventlog/EventLog/StoreId">;
        readonly startSequence: number;
    }, options?: {
        readonly asQueue?: AsQueue | undefined;
        readonly streamBufferSize?: number | undefined;
        readonly headers?: import("../http/Headers.ts").Input | undefined;
        readonly context?: Context.Context<never> | undefined;
    } | undefined) => AsQueue extends true ? Effect.Effect<Queue.Dequeue<ChunkedMessage | import("./EventLogMessage.ts").SingleMessage, import("../Cause.ts").Done<void> | EventLogProtocolError | RpcClientError>, never, Scope.Scope> : import("../Stream.ts").Stream<ChunkedMessage | import("./EventLogMessage.ts").SingleMessage, EventLogProtocolError | RpcClientError, never>;
    readonly "EventLog.Hello": <const AsQueue extends boolean = false, const Discard = false>(input: void, options?: {
        readonly headers?: import("../http/Headers.ts").Input | undefined;
        readonly context?: Context.Context<never> | undefined;
        readonly discard?: Discard | undefined;
    } | undefined) => Effect.Effect<Discard extends true ? void : HelloResponse, RpcClientError | (Discard extends true ? never : never), never>;
    readonly "EventLog.WriteChunked": <const AsQueue extends boolean = false, const Discard = false>(input: {
        readonly _tag?: "Chunked";
        readonly id: number;
        readonly part: readonly [number, number];
        readonly data: Uint8Array<ArrayBuffer>;
    }, options?: {
        readonly headers?: import("../http/Headers.ts").Input | undefined;
        readonly context?: Context.Context<never> | undefined;
        readonly discard?: Discard | undefined;
    } | undefined) => Effect.Effect<Discard extends true ? void : void, EventLogProtocolError | RpcClientError | (Discard extends true ? never : EventLogProtocolError), never>;
    readonly "EventLog.WriteSingle": <const AsQueue extends boolean = false, const Discard = false>(input: {
        readonly data: Uint8Array<ArrayBuffer>;
    }, options?: {
        readonly headers?: import("../http/Headers.ts").Input | undefined;
        readonly context?: Context.Context<never> | undefined;
        readonly discard?: Discard | undefined;
    } | undefined) => Effect.Effect<Discard extends true ? void : void, EventLogProtocolError | RpcClientError | (Discard extends true ? never : EventLogProtocolError), never>;
}>;
/**
 * Service that provides a typed RPC client for the `EventLogRemoteRpcs` protocol.
 *
 * **When to use**
 *
 * Use to provide the RPC client used by remote event-log replicas to
 * authenticate, write entries, and subscribe to changes.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class EventLogRemoteClient extends EventLogRemoteClient_base {
    static readonly layer: Layer.Layer<EventLogRemoteClient, never, RpcClient.Protocol>;
}
/**
 * Creates an `EventLogRemote` from custom write encoding and change decoding
 * functions.
 *
 * **Details**
 *
 * The remote performs the hello/authentication handshake, retries after forbidden
 * responses by re-authenticating, chunks large writes, and registers itself with
 * the `Registry` for the current scope.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeWith: (args_0: {
    readonly encodeWrite: (options: {
        readonly identity: Identity["Service"];
        readonly entries: ReadonlyArray<Entry>;
        readonly storeId: StoreId;
    }) => Effect.Effect<Uint8Array<ArrayBuffer>, Schema.SchemaError>;
    readonly decodeChanges: (identity: Identity["Service"], data: Uint8Array<ArrayBuffer>) => Effect.Effect<ReadonlyArray<RemoteEntry>, Schema.SchemaError>;
}) => Effect.Effect<{
    readonly id: RemoteId;
    readonly changes: (options: {
        readonly identity: Identity["Service"];
        readonly storeId: StoreId;
        readonly startSequence: number;
    }) => Effect.Effect<Queue.Dequeue<RemoteEntry, EventLogRemoteError>, never, Scope.Scope>;
    readonly write: (options: {
        readonly identity: Identity["Service"];
        readonly storeId: StoreId;
        readonly entries: ReadonlyArray<Entry>;
    }) => Effect.Effect<void, EventLogRemoteError>;
    readonly whenAuthenticated: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E | EventLogRemoteError, R | Identity>;
}, EventLogRemoteError, EventLogRemoteClient | Registry | Scope.Scope>;
/**
 * Creates an `EventLogRemote` that encrypts outgoing entries and decrypts
 * incoming changes with `EventLogEncryption`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeEncrypted: Effect.Effect<{
    readonly id: RemoteId;
    readonly changes: (options: {
        readonly identity: Identity["Service"];
        readonly storeId: StoreId;
        readonly startSequence: number;
    }) => Effect.Effect<Queue.Dequeue<RemoteEntry, EventLogRemoteError>, never, Scope.Scope>;
    readonly write: (options: {
        readonly identity: Identity["Service"];
        readonly storeId: StoreId;
        readonly entries: ReadonlyArray<Entry>;
    }) => Effect.Effect<void, EventLogRemoteError>;
    readonly whenAuthenticated: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E | EventLogRemoteError, R | Identity>;
}, EventLogRemoteError, EventLogEncryption | EventLogRemoteClient | Registry | Scope.Scope>;
/**
 * Creates an `EventLogRemote` that sends and receives plaintext entry payloads.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeUnencrypted: Effect.Effect<EventLogRemote["Service"], EventLogRemoteError, Scope.Scope | EventLogRemoteClient | Registry>;
/**
 * Provides an encrypted `EventLogRemote` using the remote RPC client and the
 * default Web Crypto encryption layer.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerEncrypted: Layer.Layer<EventLogRemote, EventLogRemoteError, RpcClient.Protocol | Registry>;
/**
 * Provides an unencrypted `EventLogRemote` using the remote RPC client.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerUnencrypted: Layer.Layer<EventLogRemote, EventLogRemoteError, RpcClient.Protocol | Registry>;
export {};
//# sourceMappingURL=EventLogRemote.d.ts.map