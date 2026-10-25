import { Duration, Effect, Schema, Stream } from "effect";
import { AIError } from "./schema/errors.js";
export declare const Status: Schema.Literals<readonly ["queued", "running", "completed", "failed", "cancelled", "expired"]>;
export type Status = Schema.Schema.Type<typeof Status>;
/** Provider-neutral view of one generation observation. */
export interface Snapshot {
    readonly id: string;
    readonly status: Status;
    /** Normalized 0..1 when the provider reports progress. */
    readonly progress?: number;
    readonly position?: number;
}
/**
 * Route-owned generation operations for one generation. The media route decodes its serializable token once (from the
 * submission response or a `resume` input) and closes over it, so `Generation` never sees the token's shape.
 */
export interface Route<Response> {
    readonly status: Effect.Effect<Snapshot, AIError>;
    readonly result: Effect.Effect<Response, AIError>;
    readonly cancel?: Effect.Effect<void, AIError>;
}
export interface Poll {
    readonly interval?: Duration.Input;
    readonly timeout?: Duration.Input;
}
export interface AwaitOptions {
    readonly poll?: Poll;
}
export declare const DEFAULT_POLL_INTERVAL: Duration.Duration;
export declare const DEFAULT_POLL_TIMEOUT: Duration.Duration;
export declare const QueuedEvent: Schema.Struct<{
    readonly type: Schema.tag<"generation-queued">;
    readonly id: Schema.String;
    readonly position: Schema.optional<Schema.Number>;
}>;
export declare const ProgressEvent: Schema.Struct<{
    readonly type: Schema.tag<"generation-progress">;
    readonly id: Schema.String;
    readonly progress: Schema.optional<Schema.Number>;
}>;
export type Observation = Schema.Schema.Type<typeof QueuedEvent> | Schema.Schema.Type<typeof ProgressEvent>;
export type Event = Observation | {
    readonly type: "generation-finished";
    readonly id: string;
    readonly status: Status;
};
export declare const isTerminal: (status: Status) => boolean;
export declare class Generation<Response> {
    readonly route: Route<Response>;
    /** Route-owned serializable JSON; pass it to the modality's `resume` from another process. */
    readonly token: unknown;
    readonly id: string;
    readonly status: Status;
    readonly progress?: number;
    readonly position?: number;
    constructor(route: Route<Response>, 
    /** Route-owned serializable JSON; pass it to the modality's `resume` from another process. */
    token: unknown, snapshot: Snapshot);
    get snapshot(): Snapshot;
    get terminal(): boolean;
    refresh(): Effect.Effect<Generation<Response>, AIError>;
    /** Fetch the result without polling; non-completed terminal generations fail with the provider's terminal body. */
    result(): Effect.Effect<Response, AIError>;
    /** Poll until the generation reaches a terminal status, then fetch the result. Fails with a `Timeout` reason on deadline. */
    await(options?: AwaitOptions): Effect.Effect<Response, AIError>;
    cancel(): Effect.Effect<void, AIError>;
    /**
     * Status observations as a stream, ending after the first terminal observation. Each poll and each sleep between polls
     * is bounded by the time remaining until `poll.timeout`, so a hung status request or a long interval fails the stream at
     * the deadline instead of stalling it. (`Stream.interruptWhen` would express this directly but deadlocks under
     * `TestClock` when the source completes while the timer sleeps.)
     */
    events(options?: AwaitOptions): Stream.Stream<Event, AIError>;
    private event;
    private poll;
    private schedule;
}
/** `events` followed by the expanded result, with the result fetch bounded by the same `poll.timeout` deadline. */
export declare const resultEvents: <Response, A>(generation: Generation<Response>, expand: (response: Response) => ReadonlyArray<A>, options?: AwaitOptions) => Stream.Stream<Observation | A, AIError>;
