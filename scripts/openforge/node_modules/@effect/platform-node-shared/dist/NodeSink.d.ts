/**
 * Sink adapters for writing Effect chunks into Node writable streams.
 *
 * `fromWritable` creates a `Sink`, `fromWritableChannel` creates a lower-level
 * `Channel`, and `pullIntoWritable` writes from an existing pull loop. All
 * three adapters respect writable-stream backpressure, map writable errors with
 * the supplied `onError` function, and can end the writable when the upstream
 * data is done.
 *
 * @since 4.0.0
 */
import type { NonEmptyReadonlyArray } from "effect/Array";
import * as Channel from "effect/Channel";
import { type LazyArg } from "effect/Function";
import * as Pull from "effect/Pull";
import * as Sink from "effect/Sink";
import type { Writable } from "node:stream";
/**
 * Creates a `Sink` that writes chunks to a Node writable stream, respecting
 * backpressure, mapping writable errors with `onError`, and ending the stream
 * on completion unless `endOnDone` is `false`.
 *
 * @category constructors
 * @since 4.0.0
 */
export declare const fromWritable: <E, A = Uint8Array | string>(options: {
    readonly evaluate: LazyArg<Writable | NodeJS.WritableStream>;
    readonly onError: (error: unknown) => E;
    readonly endOnDone?: boolean | undefined;
    readonly encoding?: BufferEncoding | undefined;
}) => Sink.Sink<void, A, never, E>;
/**
 * Creates a `Channel` that pulls chunks from upstream and writes them to a
 * Node writable stream, respecting backpressure and optionally ending the
 * writable when upstream is done.
 *
 * @category constructors
 * @since 4.0.0
 */
export declare const fromWritableChannel: <IE, E, A = Uint8Array | string>(options: {
    readonly evaluate: LazyArg<Writable | NodeJS.WritableStream>;
    readonly onError: (error: unknown) => E;
    readonly endOnDone?: boolean | undefined;
    readonly encoding?: BufferEncoding | undefined;
}) => Channel.Channel<never, IE | E, void, NonEmptyReadonlyArray<A>, IE>;
/**
 * Writes Effect chunks into a Node writable stream.
 *
 * **When to use**
 *
 * Use to implement custom Node stream adapters that already have an upstream
 * pull and need direct control over a writable stream.
 *
 * **Details**
 *
 * The loop waits for `drain` when needed, fails on writable errors, and ends
 * the writable on upstream completion unless `endOnDone` is `false`.
 *
 * @category converting
 * @since 4.0.0
 */
export declare const pullIntoWritable: <A, IE, E>(options: {
    readonly pull: Pull.Pull<NonEmptyReadonlyArray<A>, IE, unknown>;
    readonly writable: Writable;
    readonly onError: (error: unknown) => E;
    readonly endOnDone?: boolean | undefined;
    readonly encoding?: BufferEncoding | undefined;
}) => Pull.Pull<never, IE | E, unknown>;
//# sourceMappingURL=NodeSink.d.ts.map