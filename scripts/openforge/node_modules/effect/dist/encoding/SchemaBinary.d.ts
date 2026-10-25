/**
 * A compact binary codec derived from the encoded side of a Schema.
 *
 * The default wire format supports compatible schema evolution. An array of
 * structs is written as a row run: rows declare their shape once and
 * back-reference repeated strings, so both stay off the wire on later rows.
 * Fingerprint mode uses positional layouts and rejects mismatches for smaller
 * frames; its row shapes are presence masks instead of field id lists. Encoded
 * results are arena-backed views; see {@link toCodec} for ownership details.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.ts";
import * as Channel from "../Channel.ts";
import * as Effect from "../Effect.ts";
import * as Schema from "../Schema.ts";
import * as SchemaAST from "../SchemaAST.ts";
/**
 * Selects the wire mode.
 *
 * **Details**
 *
 * The default mode supports compatible schema evolution. `fingerprint: true`
 * uses positional layouts and an 8-byte layout hash for smaller frames, but
 * requires peers to use the same schema definition.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Options {
    /**
     * Uses a compact positional layout with a schema fingerprint.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly fingerprint?: boolean | undefined;
}
/**
 * The codec type returned by {@link toCodec}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface toCodec<S extends Schema.Constraint> extends Schema.Codec<S["Type"], Uint8Array<ArrayBuffer>, S["DecodingServices"], S["EncodingServices"]> {
}
/**
 * Derives a compact binary codec from a schema.
 *
 * **Details**
 *
 * The wire layout is compiled from the encoded side of the schema. Each
 * encode/decode handles exactly one frame; use {@link parser} for streams.
 * Derived codecs are memoized by schema identity and wire mode.
 *
 * Encoded results are arena-backed views and may share a larger buffer. Use
 * `bytes.slice()` when independent ownership is required.
 *
 * **Example** (Deriving a binary codec)
 *
 * ```ts
 * import { Schema } from "effect"
 * import { SchemaBinary } from "effect/encoding"
 *
 * const Person = Schema.Struct({ name: Schema.String, age: Schema.Number })
 * const codec = SchemaBinary.toCodec(Person)
 *
 * const bytes = Schema.encodeUnknownSync(codec)({ name: "Ada", age: 36 })
 * const person = Schema.decodeUnknownSync(codec)(bytes)
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare function toCodec<S extends Schema.Constraint>(schema: S, options?: Options): toCodec<S>;
/**
 * A stateful frame parser for concatenated {@link toCodec} outputs.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Parser<T> {
    /**
     * Feeds another chunk into the parser and returns any completed values.
     *
     * @stability unstable
     * @since 4.0.0
     */
    feed(chunk: Uint8Array): Effect.Effect<ReadonlyArray<T>, Schema.SchemaError>;
    /**
     * Feeds another chunk into the parser synchronously.
     *
     * @stability unstable
     * @since 4.0.0
     */
    feedSync(chunk: Uint8Array): ReadonlyArray<T>;
    /**
     * Finishes parsing and fails if an incomplete frame remains.
     *
     * @stability unstable
     * @since 4.0.0
     */
    end: Effect.Effect<void, Schema.SchemaError>;
    /**
     * Finishes parsing synchronously and throws for an incomplete frame.
     *
     * @stability unstable
     * @since 4.0.0
     */
    endSync(): void;
}
/**
 * Creates a stateful parser for a stream of concatenated frames.
 *
 * **Details**
 *
 * Values completed before a failure remain observable. After a failure, the
 * parser rejects further calls. Use `maxFrameSize` to limit buffered frames.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare function parser<S extends Schema.Constraint>(schema: S, options?: SchemaAST.ParseOptions & Options & StreamOptions & {
    readonly maxFrameSize?: number | undefined;
}): Parser<S["Type"]>;
/**
 * Options for the connection-scoped pair, {@link encoder} and {@link parser}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface StreamOptions {
    /**
     * Share one string dictionary across every frame on the connection, so a
     * string that repeats costs a reference after the first frame that carries
     * it. Frames stop standing alone: they only decode through the parser that
     * saw the frames before them, in order.
     *
     * **Gotchas**
     *
     * Both ends have to set it. A schema the binary layer does not fully
     * validate on its own cannot use it, and asking for it throws.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly dictionary?: boolean | undefined;
}
/**
 * A writer for one connection. Every frame it produces shares the string
 * dictionary that the matching {@link parser} rebuilds as it reads them, so the
 * two have to be created from the same schema and the same options.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare function encoder<S extends Schema.Constraint>(schema: S, options?: SchemaAST.ParseOptions & Options & StreamOptions): Encoder;
/**
 * The writer returned by {@link encoder}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Encoder {
    encode(value: unknown): Uint8Array<ArrayBuffer>;
    encodeMany(values: ReadonlyArray<unknown>): Uint8Array<ArrayBuffer>;
}
/**
 * Creates a channel that encodes chunks of schema values into binary frames.
 *
 * **Details**
 *
 * Each input chunk is emitted as one byte element holding concatenated frames
 * written in one writer pass, so a batch costs no more allocations than a
 * single frame. Schemas with transformations first run one schema pass per
 * chunk to the binary-adjusted encoded side — supporting async transformations
 * and encoding services — before that writer pass. Failures are
 * `Schema.SchemaError`, matching {@link toCodec}. `maxFrameSize` only applies
 * to {@link decode} and is ignored here.
 *
 * @stability unstable
 * @category channels
 * @since 4.0.0
 */
export declare const encode: <S extends Schema.Constraint>(schema: S, options?: SchemaAST.ParseOptions & Options & {
    readonly maxFrameSize?: number | undefined;
}) => <IE = never, Done = unknown>() => Channel.Channel<Arr.NonEmptyReadonlyArray<Uint8Array<ArrayBuffer>>, Schema.SchemaError | IE, Done, Arr.NonEmptyReadonlyArray<S["Type"]>, IE, Done, S["EncodingServices"]>;
/**
 * Creates a channel that decodes chunks of binary frames into schema values.
 *
 * **Details**
 *
 * The channel keeps one frame parser for its lifetime: frames may be
 * fragmented across chunks or concatenated within one, values completed before
 * a failure remain observable, and a leftover incomplete frame fails when the
 * upstream is done. Schemas with transformations run the schema pass per
 * framed value, which supports async transformations and decoding services.
 * Failures are `Schema.SchemaError`, matching {@link toCodec}.
 *
 * @stability unstable
 * @category channels
 * @since 4.0.0
 */
export declare const decode: <S extends Schema.Constraint>(schema: S, options?: SchemaAST.ParseOptions & Options & {
    readonly maxFrameSize?: number | undefined;
}) => <IE = never, Done = unknown>() => Channel.Channel<Arr.NonEmptyReadonlyArray<S["Type"]>, Schema.SchemaError | IE, Done, Arr.NonEmptyReadonlyArray<Uint8Array<ArrayBuffer>>, IE, Done, S["DecodingServices"]>;
/**
 * Wraps a bidirectional byte channel with schema-driven binary encoding and
 * decoding.
 *
 * **Details**
 *
 * Values sent to the wrapped channel are encoded with `inputSchema` as binary
 * frames; bytes received from it are decoded with `outputSchema`.
 *
 * @stability unstable
 * @category channels
 * @since 4.0.0
 */
export declare const duplex: {
    /**
     * Wraps a bidirectional byte channel with schema-driven binary encoding and
     * decoding.
     *
     * **Details**
     *
     * Values sent to the wrapped channel are encoded with `inputSchema` as binary
     * frames; bytes received from it are decoded with `outputSchema`.
     *
     * @stability unstable
     * @category channels
     * @since 4.0.0
     */
    <In extends Schema.Constraint, Out extends Schema.Constraint>(options: SchemaAST.ParseOptions & Options & {
        readonly inputSchema: In;
        readonly outputSchema: Out;
        readonly maxFrameSize?: number | undefined;
    }): <OutErr, OutDone, InErr, InDone, R>(self: Channel.Channel<Arr.NonEmptyReadonlyArray<Uint8Array<ArrayBuffer>>, OutErr, OutDone, Arr.NonEmptyReadonlyArray<Uint8Array<ArrayBuffer>>, Schema.SchemaError | InErr, InDone, R>) => Channel.Channel<Arr.NonEmptyReadonlyArray<Out["Type"]>, Schema.SchemaError | OutErr, OutDone, Arr.NonEmptyReadonlyArray<In["Type"]>, InErr, InDone, R | In["EncodingServices"] | Out["DecodingServices"]>;
    /**
     * Wraps a bidirectional byte channel with schema-driven binary encoding and
     * decoding.
     *
     * **Details**
     *
     * Values sent to the wrapped channel are encoded with `inputSchema` as binary
     * frames; bytes received from it are decoded with `outputSchema`.
     *
     * @stability unstable
     * @category channels
     * @since 4.0.0
     */
    <Out extends Schema.Constraint, In extends Schema.Constraint, OutErr, OutDone, InErr, InDone, R>(self: Channel.Channel<Arr.NonEmptyReadonlyArray<Uint8Array<ArrayBuffer>>, OutErr, OutDone, Arr.NonEmptyReadonlyArray<Uint8Array<ArrayBuffer>>, Schema.SchemaError | InErr, InDone, R>, options: SchemaAST.ParseOptions & Options & {
        readonly inputSchema: In;
        readonly outputSchema: Out;
        readonly maxFrameSize?: number | undefined;
    }): Channel.Channel<Arr.NonEmptyReadonlyArray<Out["Type"]>, Schema.SchemaError | OutErr, OutDone, Arr.NonEmptyReadonlyArray<In["Type"]>, InErr, InDone, R | In["EncodingServices"] | Out["DecodingServices"]>;
};
/**
 * Assigns an explicit wire field id to a struct property.
 *
 * **When to use**
 *
 * Use to preserve the wire id across a rename or resolve a hash collision.
 *
 * **Details**
 *
 * Valid ids are integers from 1 through 4294967295.
 *
 * **Example** (Assigning a wire field id)
 *
 * ```ts
 * import { Schema } from "effect"
 * import { SchemaBinary } from "effect/encoding"
 *
 * const Person = Schema.Struct({
 *   id: Schema.String.pipe(SchemaBinary.fieldId(1))
 * })
 * ```
 *
 * @stability unstable
 * @category annotations
 * @since 4.0.0
 */
export declare function fieldId(id: number): <S extends Schema.Top>(self: S) => S["Rebuild"];
//# sourceMappingURL=SchemaBinary.d.ts.map