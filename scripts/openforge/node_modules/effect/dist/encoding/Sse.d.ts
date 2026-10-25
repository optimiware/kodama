/**
 * Parses and renders Server-Sent Events text streams.
 *
 * Server-Sent Events, or SSE, are the text format used by `EventSource` for
 * one-way server-to-client updates. This module includes parsers, encoders,
 * channel helpers, and schema-based helpers for the `id`, `event`, and `data`
 * fields of each event.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type { NonEmptyReadonlyArray } from "../Array.ts";
import * as Cause from "../Cause.ts";
import * as Channel from "../Channel.ts";
import * as Duration from "../Duration.ts";
import * as Result from "../Result.ts";
import * as Schema from "../Schema.ts";
import type * as SchemaAST from "../SchemaAST.ts";
import * as SchemaTransformation from "../SchemaTransformation.ts";
declare const SseErrorTypeId = "~effect/encoding/Sse/SseError";
declare const EventTooLarge_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => Cause.YieldableError & {
    readonly _tag: "EventTooLarge";
} & Readonly<A>;
/**
 * Error reason raised when pending Server-Sent Events state exceeds the
 * configured maximum size.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class EventTooLarge extends EventTooLarge_base<{
    readonly maxEventSize: number;
}> {
    get message(): string;
}
/**
 * Union of Server-Sent Events decoding error reasons.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type SseErrorReason = EventTooLarge;
declare const SseError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => Cause.YieldableError & {
    readonly _tag: "SseError";
} & Readonly<A>;
/**
 * Error raised when decoding a Server-Sent Events stream fails.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class SseError extends SseError_base<{
    readonly reason: SseErrorReason;
}> {
    /**
     * Marks this value as an SSE decoding error.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [SseErrorTypeId] = "~effect/encoding/Sse/SseError";
    /**
     * Delegates the public message to the underlying SSE error reason.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
/**
 * Options for decoding Server-Sent Events streams.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export interface DecodeOptions {
    /**
     * Maximum number of string code units retained for a pending event. The default is 10 MiB.
     */
    readonly maxEventSize?: number | undefined;
}
/**
 * Creates a channel that parses Server-Sent Events text chunks into `Event` values.
 *
 * **Details**
 *
 * SSE `retry` directives are emitted as `Retry` failures so callers can
 * reconnect with the requested delay.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const decode: <IE, Done>(options?: DecodeOptions) => Channel.Channel<NonEmptyReadonlyArray<Event>, IE | Retry | SseError, Done, NonEmptyReadonlyArray<string>, IE, Done>;
/**
 * A constraint for schemas that can decode SSE events.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export interface EventCodec extends Schema.ConstraintCodec<any, {
    readonly id?: string | undefined;
    readonly event?: string | undefined;
    readonly data: string;
}, unknown, unknown> {
}
/**
 * Creates an SSE decoder channel that decodes each parsed event with a schema.
 *
 * **Details**
 *
 * The schema receives `{ event, data, id? }`, with string `data`. Absent IDs are
 * omitted, even with default options: use `Schema.optional(Schema.String)`, not
 * `Schema.UndefinedOr(Schema.String)`, to accept events without an ID.
 *
 * With `onExcessProperty: "error"`, declare `event` (default: `"message"`) and
 * `id` if the stream carries IDs, including inherited IDs.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const decodeSchema: <S extends EventCodec, IE, Done>(schema: S, options?: DecodeOptions, parseOptions?: SchemaAST.ParseOptions) => Channel.Channel<NonEmptyReadonlyArray<S["Type"]>, IE | Retry | SseError | Schema.SchemaError, Done, NonEmptyReadonlyArray<string>, IE, Done, S["DecodingServices"]>;
/**
 * Creates an SSE decoder channel that JSON-decodes each event `data` field with a schema.
 *
 * **Details**
 *
 * The output preserves the SSE `event` name and optional `id` while replacing
 * `data` with the decoded value.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const decodeDataSchema: <Type, DecodingServices, IE, Done>(schema: Schema.ConstraintDecoder<Type, DecodingServices>, options?: DecodeOptions) => Channel.Channel<NonEmptyReadonlyArray<{
    readonly event: string;
    readonly id: string | undefined;
    readonly data: Type;
}>, IE | Retry | SseError | Schema.SchemaError, Done, NonEmptyReadonlyArray<string>, IE, Done, DecodingServices>;
/**
 * Creates a stateful Server-Sent Events parser.
 *
 * **Details**
 *
 * Call `feed` with text chunks to parse `Event` and `Retry` values through the
 * callback, and call `reset` to clear any buffered event state. `feed` returns
 * an `SseError` if the pending event exceeds `maxEventSize`.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare function makeParser(onParse: (event: AnyEvent) => void, options?: DecodeOptions): Parser;
/**
 * Stateful Server-Sent Events parser returned by `makeParser`.
 *
 * **Details**
 *
 * `feed` accepts additional text chunks and returns an `SseError` when the
 * configured pending event size is exceeded. `reset` clears buffered parser
 * state.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export interface Parser {
    feed(chunk: string): SseError | undefined;
    reset(): void;
}
/**
 * Creates a channel that encodes `Event` values as Server-Sent Events text.
 *
 * **Details**
 *
 * If the upstream channel fails with `Retry`, the retry directive is written and
 * the encoder completes.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare const encode: <IE, Done>() => Channel.Channel<NonEmptyReadonlyArray<string>, IE, void, NonEmptyReadonlyArray<Event>, IE | Retry, Done>;
/**
 * Creates an SSE encoder channel for values accepted by a schema.
 *
 * **Details**
 *
 * Values are schema-encoded to the untagged SSE event shape, transformed to
 * `Event`, and then written as Server-Sent Events text.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare const encodeSchema: <S extends EventCodec, IE, Done>(schema: S) => Channel.Channel<NonEmptyReadonlyArray<string>, IE | Schema.SchemaError, void, NonEmptyReadonlyArray<S["Type"]>, IE | Retry, Done, S["EncodingServices"]>;
/**
 * Encoder capable of rendering an `Event` or `Retry` value as Server-Sent
 * Events text.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export interface Encoder {
    write(event: AnyEvent): string;
}
/**
 * Tagged model for a Server-Sent Events message containing the event name, optional event ID, and string data payload.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Event {
    readonly _tag: "Event";
    readonly event: string;
    readonly id: string | undefined;
    readonly data: string;
}
/**
 * Schema for the untagged Server-Sent Events payload shape containing an optional `id`, `event`, and string `data` fields.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare const EventEncoded: Schema.Struct<{
    readonly id: Schema.optional<Schema.String>;
    readonly event: Schema.String;
    readonly data: Schema.String;
}>;
/**
 * Schema for the tagged Server-Sent Events message model that adds `_tag: "Event"` to the event name, optional event ID, and string data payload.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare const Event: Schema.Struct<{
    readonly _tag: Schema.tag<"Event">;
    readonly id: Schema.UndefinedOr<Schema.String>;
    readonly event: Schema.String;
    readonly data: Schema.String;
}>;
/**
 * Schema for transforming untagged SSE event payloads into tagged `Event`
 * models.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare const transformEvent: SchemaTransformation.Transformation<{
    readonly id?: string | undefined;
    readonly event?: string | undefined;
    readonly data: string;
}, {
    readonly _tag: "Event";
    readonly id: string | undefined;
    readonly event: string;
    readonly data: string;
}, never, never>;
/**
 * Untagged Server-Sent Events payload shape containing the event name, optional event ID, and string data payload.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface EventEncoded {
    readonly event: string;
    readonly id?: string | undefined;
    readonly data: string;
}
declare const RetryTypeId: "~effect/encoding/Sse/Retry";
declare const Retry_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => Readonly<A> & {
    readonly _tag: "Retry";
} & import("../Pipeable.ts").Pipeable;
/**
 * Represents a Server-Sent Events retry directive.
 *
 * **Details**
 *
 * Decoders surface this value as a failure to request reconnection after
 * `duration`; encoders serialize an upstream `Retry` failure as a `retry:` line.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class Retry extends Retry_base<{
    readonly duration: Duration.Duration;
    readonly lastEventId: string | undefined;
}> {
    /**
     * Marks this value as an SSE retry directive for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [RetryTypeId]: typeof RetryTypeId;
    /**
     * Returns `true` when the value is an SSE retry directive.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static is(u: unknown): u is Retry;
    /**
     * Separates SSE retry directives from regular event values.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static filter<A>(u: A): Result.Result<Retry, Exclude<A, Retry>>;
}
/**
 * Union of SSE values that can be rendered by an `Encoder`: regular events and
 * retry directives.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type AnyEvent = Event | Retry;
/**
 * Default Server-Sent Events encoder.
 *
 * **Details**
 *
 * It renders `Event` values as `id`, `event`, and `data` lines and renders
 * `Retry` values as `retry:` directives.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare const encoder: Encoder;
export {};
//# sourceMappingURL=Sse.d.ts.map