export * as RpcClientRuntime from "./rpc.js";
import type { Rpc } from "@opencode/schema/rpc";
import type { RpcError, RpcInternalError } from "@opencode/protocol/errors";
import type { OpenCodeEvent } from "@opencode/protocol/groups/event";
import { Effect, Schema, Stream } from "effect";
import type { RpcArguments, RpcCallOptions } from "../promise/rpc.js";
import type { RpcCallInput, RpcCallOutput } from "./api/api.js";
type DecodeError<S> = S extends Schema.Top ? Schema.SchemaError : never;
export type RpcClient<D extends Rpc.Definition, E = never, Options = RpcCallOptions, EventError = E> = {
    readonly [Name in keyof D["methods"]]: (...args: RpcArguments<Rpc.Input<D["methods"][Name]["input"]>, Options>) => Effect.Effect<Rpc.Output<D["methods"][Name]["output"]>, Rpc.MethodError<D["methods"][Name]> | DecodeError<D["methods"][Name]["output"]> | E>;
} & {
    readonly events: {
        readonly subscribe: <Name extends keyof D["events"] & string>(name: Name) => Stream.Stream<Rpc.EventPayload<D, Name>, DecodeError<D["events"][Name]["schema"]> | EventError>;
    };
};
export interface RpcApi<E = never, Options = RpcCallOptions, EventError = E> {
    <D extends Rpc.Definition>(definition: D): RpcClient<D, E, Options, EventError>;
}
export declare function make<CallError, EventError>(call: (input: RpcCallInput, options?: RpcCallOptions) => Effect.Effect<RpcCallOutput, CallError>, subscribe: () => Stream.Stream<OpenCodeEvent, EventError>): RpcApi<Exclude<CallError, RpcError | RpcInternalError> | Rpc.SystemError, RpcCallOptions, EventError>;
export declare function aborted(signal: AbortSignal): Effect.Effect<void, never, never>;
