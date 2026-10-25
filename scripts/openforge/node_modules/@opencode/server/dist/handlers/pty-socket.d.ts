import { Effect, Queue } from "effect";
import { Socket } from "effect/socket";
export type Outbound = string | Uint8Array | Socket.CloseEvent;
export declare function runPtySocket<R>(input: {
    readonly socket: Socket.Socket;
    readonly outbox: Queue.Dequeue<Outbound>;
    readonly onOpen?: Effect.Effect<void, never, R>;
    readonly onMessage: (message: string | Uint8Array) => Effect.Effect<void, never, R>;
    readonly detach: () => void;
}): Effect.Effect<void, never, R | import("effect/Scope").Scope>;
