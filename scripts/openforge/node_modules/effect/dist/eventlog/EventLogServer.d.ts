import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import type * as Rpc from "../rpc/Rpc.ts";
import type * as RpcGroup from "../rpc/RpcGroup.ts";
import * as Stream from "../Stream.ts";
import type { RemoteId } from "./EventJournal.ts";
import { EventLogAuthentication, EventLogProtocolError, EventLogRemoteRpcs, type StoreId } from "./EventLogMessage.ts";
/**
 * Provides RPC authentication middleware that reads the authenticated
 * `EventLog.Identity` from client annotations.
 *
 * **Details**
 *
 * Requests without an identity fail with a forbidden `EventLogProtocolError`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerAuthMiddleware: Layer.Layer<EventLogAuthentication>;
/**
 * Creates the shared RPC handlers for the event-log remote protocol.
 *
 * **Details**
 *
 * The layer manages hello challenges, verifies session authentication, reassembles
 * chunked writes, delegates write and change handling to the supplied callbacks,
 * and frames large change payloads into chunks.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerRpcHandlers: (options: {
    readonly remoteId: RemoteId;
    readonly authorizeIdentity?: (publicKey: string) => Effect.Effect<void, EventLogProtocolError>;
    readonly getOrCreateSessionAuthBinding: (publicKey: string, signingPublicKey: Uint8Array<ArrayBuffer>) => Effect.Effect<Uint8Array<ArrayBuffer>>;
    readonly onWrite: (data: Uint8Array<ArrayBuffer>, authenticatedPublicKeys: ReadonlySet<string>) => Effect.Effect<void, EventLogProtocolError>;
    readonly changes: (options: {
        readonly publicKey: string;
        readonly storeId: StoreId;
        readonly startSequence: number;
    }) => Stream.Stream<Uint8Array<ArrayBuffer>, unknown>;
}) => Layer.Layer<Rpc.ToHandler<RpcGroup.Rpcs<typeof EventLogRemoteRpcs>> | EventLogAuthentication>;
declare const ChunkedMessageState_base: Context.Reference<Map<number, {
    readonly parts: Array<Uint8Array>;
    count: number;
    bytes: number;
}>>;
/**
 * Annotation that stores partial `ChunkedMessage` data while chunked writes are
 * being reassembled.
 *
 * **When to use**
 *
 * Use to keep per-client chunk assembly state while handling chunked event-log
 * writes.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class ChunkedMessageState extends ChunkedMessageState_base {
}
declare const AuthenticatedIdentities_base: Context.ServiceClass<AuthenticatedIdentities, "effect/eventlog/EventLogServer/AuthenticatedIdentities", Set<string>>;
/**
 * Annotation containing the public keys authenticated on an RPC connection.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class AuthenticatedIdentities extends AuthenticatedIdentities_base {
}
export {};
//# sourceMappingURL=EventLogServer.d.ts.map