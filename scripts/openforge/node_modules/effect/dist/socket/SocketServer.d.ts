/**
 * Service contract for servers that accept socket connections.
 *
 * `SocketServer` exposes the bound server `address` and a long-running `run`
 * loop that hands each accepted connection to a handler as a `Socket.Socket`.
 * Bound addresses use the shared `NetAddress.SocketAddress` model. This module also
 * defines server-level errors reported while opening or running a server.
 * Platform layers provide concrete implementations of this service.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.ts";
import type * as Effect from "../Effect.ts";
import type * as NetAddress from "../net/NetAddress.ts";
import type * as Socket from "./Socket.ts";
declare const SocketServer_base: Context.ServiceClass<SocketServer, "@effect/platform/SocketServer", {
    readonly address: NetAddress.SocketAddress;
    readonly run: <R, E, _>(handler: (socket: Socket.Socket) => Effect.Effect<_, E, R>) => Effect.Effect<never, SocketServerError, R>;
}>;
/**
 * Context service for a socket server, exposing its bound address and a run
 * loop that handles each accepted `Socket`.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class SocketServer extends SocketServer_base {
}
/**
 * Runtime type identifier attached to `SocketServerError` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const ErrorTypeId: ErrorTypeId;
/**
 * Type-level identifier used to mark `SocketServerError` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type ErrorTypeId = "@effect/platform/SocketServer/SocketServerError";
declare const SocketServerOpenError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "SocketServerOpenError";
} & Readonly<A>;
/**
 * Error reason for failures that occur while opening a socket server.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketServerOpenError extends SocketServerOpenError_base<{
    readonly cause: unknown;
}> {
    get message(): string;
}
declare const SocketServerUnknownError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "SocketServerUnknownError";
} & Readonly<A>;
/**
 * Error reason for uncategorized socket server failures.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketServerUnknownError extends SocketServerUnknownError_base<{
    readonly cause: unknown;
}> {
    get message(): string;
}
/**
 * Union of socket server error reasons.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type SocketServerErrorReason = SocketServerOpenError | SocketServerUnknownError;
declare const SocketServerError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "SocketServerError";
} & Readonly<A>;
/**
 * Tagged socket server error that wraps a server error reason and exposes its
 * cause.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SocketServerError extends SocketServerError_base<{
    readonly reason: SocketServerErrorReason;
}> {
    constructor(props: {
        readonly reason: SocketServerErrorReason;
    });
    /**
     * Marks this value as a socket server error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ErrorTypeId]: ErrorTypeId;
    /**
     * Delegates the public message to the underlying socket server error reason.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
export {};
//# sourceMappingURL=SocketServer.d.ts.map