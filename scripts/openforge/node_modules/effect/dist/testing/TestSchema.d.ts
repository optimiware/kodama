import * as Arbitrary from "../Arbitrary.ts";
import type * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Schema from "../Schema.ts";
import * as SchemaAST from "../SchemaAST.ts";
import * as SchemaIssue from "../SchemaIssue.ts";
/**
 * Provides schema test assertions for decoding, encoding, make, arbitrary generation, and round-trip verification.
 *
 * **When to use**
 *
 * Use when writing schema unit tests for decoding, encoding, construction, property-based round-trip, or generation behavior.
 *
 * **Example** (Decoding and encoding a struct)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { TestSchema } from "effect/testing"
 *
 * const schema = Schema.Struct({ name: Schema.String })
 * const asserts = new TestSchema.Asserts(schema)
 *
 * // decoding
 * await asserts.decoding().succeed({ name: "Alice" }) // => undefined
 *
 * // encoding
 * await asserts.encoding().succeed({ name: "Alice" }) // => undefined
 * ```
 *
 * @see {@link Decoding}
 * @see {@link Encoding}
 * @stability unstable
 * @category testing
 * @since 4.0.0
 */
export declare class Asserts<S extends Schema.Constraint> {
    /**
     * Static helpers for comparing schema AST structures.
     *
     * **When to use**
     *
     * Use to assert that two schema field or tuple element definitions produce
     * the same AST structure.
     *
     * **Details**
     *
     * `ast.fields.equals(a, b)` compares struct field ASTs via `assert.deepStrictEqual`. `ast.elements.equals(a, b)` compares tuple element ASTs via `assert.deepStrictEqual`.
     *
     * **Example** (Comparing struct fields)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const fieldsA = { name: Schema.String }
     * const fieldsB = { name: Schema.String }
     * TestSchema.Asserts.ast.fields.equals(fieldsA, fieldsB) // => undefined
     * ```
     *
     * @stability unstable
     */
    static ast: {
        /**
         * Groups schema field comparisons.
         *
         * @stability unstable
         */
        readonly fields: {
            /**
             * Asserts that schema fields have equal AST structures.
             *
             * @stability unstable
             */
            readonly equals: (a: Schema.Struct.Fields, b: Schema.Struct.Fields) => void;
        };
        /**
         * Groups tuple element comparisons.
         *
         * @stability unstable
         */
        readonly elements: {
            /**
             * Asserts that tuple elements have equal AST structures.
             *
             * @stability unstable
             */
            readonly equals: (a: Schema.Tuple.Elements, b: Schema.Tuple.Elements) => void;
        };
    };
    /**
     * Schema used by these assertions.
     *
     * @stability unstable
     */
    readonly schema: S;
    /**
     * Creates assertions for a schema.
     *
     * @stability unstable
     */
    constructor(schema: S);
    /**
     * Returns Promise and Effect assertions for the schema's `make` operation.
     *
     * **When to use**
     *
     * Use to assert how `Schema.make` accepts, transforms, or rejects
     * construction input for this schema.
     *
     * **Details**
     *
     * `succeed(input)` asserts make returns the input unchanged. `succeed(input, expected)` asserts make returns `expected`. `fail(input, message)` asserts make fails with `message`.
     * `succeedEffect` and `failEffect` return lazy effects with the same assertions.
     * Assertion mismatches are defects in the returned effects.
     *
     * **Example** (Testing make)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const schema = Schema.String
     * const asserts = new TestSchema.Asserts(schema)
     * await asserts.make().succeed("hello") // => undefined
     * ```
     *
     * @see {@link decoding} for assertions against decoded input
     * @see {@link encoding} for assertions against encoded output
     * @stability unstable
     */
    make(options?: Schema.MakeOptions): {
        /**
         * Asserts that construction succeeds with the input or the explicit expected value.
         *
         * @stability unstable
         */
        succeed: {
            (input: S["Type"]): Promise<void>;
            (input: S["~type.make.in"], expected: S["Type"]): Promise<void>;
        };
        /**
         * Returns a lazy construction success assertion whose mismatches are defects.
         *
         * @stability unstable
         */
        succeedEffect: {
            (input: S["Type"]): Effect.Effect<void>;
            (input: S["~type.make.in"], expected: S["Type"]): Effect.Effect<void>;
        };
        /**
         * Returns a lazy assertion that construction fails with the expected issue message.
         *
         * @stability unstable
         */
        failEffect: (input: unknown, message: string) => Effect.Effect<void>;
        /**
         * Asserts that construction fails with the expected issue message.
         *
         * @stability unstable
         */
        fail(input: unknown, message: string): Promise<void>;
    };
    /**
     * Runs a property-based test that encodes arbitrary values and then decodes them, asserting the decoded value equals the original.
     *
     * **When to use**
     *
     * Use to verify that generated schema values survive an encode-then-decode
     * round trip.
     *
     * **Details**
     *
     * The native Schema arbitrary generates values matching the schema's `Type`. The assertion fails with the minimized
     * shrunk input and replay token if any generated value does not round-trip.
     *
     * **Example** (Verifying round trips)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const asserts = new TestSchema.Asserts(Schema.String)
     * await asserts.verifyRoundTrip({ seed: 1, runs: 20 }) // => undefined
     * ```
     *
     * @see {@link arbitrary} for checking that generated values satisfy the schema
     * @see {@link verifyRoundTripEffect} for the Effect variant
     * @stability unstable
     */
    verifyRoundTrip<S extends Schema.ConstraintCodec<unknown, unknown>>(this: Asserts<S>, options?: Arbitrary.CheckOptions): Promise<void>;
    /**
     * Returns a lazy property-based assertion that generated values survive an
     * encode-then-decode round trip.
     *
     * **When to use**
     *
     * Use to check round trips with the calling fiber's decoding and encoding services.
     *
     * **Details**
     *
     * Assertion mismatches are defects containing the shrunk input and replay token.
     * Schema defects and interruption propagate without becoming assertion failures.
     *
     * **Example** (Composing round-trip assertions)
     *
     * ```ts import.meta.vitest
     * import { Effect, Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const asserts = new TestSchema.Asserts(Schema.NumberFromString)
     * const test = Effect.gen(function*() {
     *   yield* asserts.decoding().succeedEffect("42", 42)
     *   yield* asserts.verifyRoundTripEffect({ seed: 1, runs: 20 })
     * })
     * await Effect.runPromise(test) // => undefined
     * ```
     *
     * @see {@link verifyRoundTrip} for the Promise variant
     * @stability unstable
     */
    verifyRoundTripEffect(options?: Arbitrary.CheckOptions): Effect.Effect<void, never, S["DecodingServices"] | S["EncodingServices"]>;
    /**
     * Returns a {@link Decoding} instance for this schema with helpers for decoding assertions.
     *
     * **When to use**
     *
     * Use to test how unknown input is decoded into the schema's type.
     *
     * **Details**
     *
     * Pass `parseOptions` to control error reporting, for example `{ errors: "all" }`.
     *
     * **Example** (Decoding assertions)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const asserts = new TestSchema.Asserts(Schema.NumberFromString)
     * const decoding = asserts.decoding()
     * await decoding.succeed("42", 42) // => undefined
     * await decoding.fail(null, "Expected string") // => undefined
     * ```
     *
     * @see {@link Decoding}
     * @see {@link encoding} for assertions in the opposite direction
     * @stability unstable
     */
    decoding(options?: {
        readonly parseOptions?: SchemaAST.ParseOptions | undefined;
    }): Decoding<S>;
    /**
     * Returns an {@link Encoding} instance for this schema with helpers for encoding assertions.
     *
     * **When to use**
     *
     * Use to test how schema values are encoded into their external form.
     *
     * **Details**
     *
     * Pass `parseOptions` to control error reporting, for example `{ errors: "all" }`.
     *
     * **Example** (Encoding assertions)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const asserts = new TestSchema.Asserts(Schema.NumberFromString)
     * const encoding = asserts.encoding()
     * await encoding.succeed(42, "42") // => undefined
     * ```
     *
     * @see {@link Encoding}
     * @see {@link decoding} for assertions in the opposite direction
     * @stability unstable
     */
    encoding(options?: {
        readonly parseOptions?: SchemaAST.ParseOptions | undefined;
    }): Encoding<S>;
    /**
     * Returns an object with property-based testing helpers for the schema's arbitrary generator.
     *
     * **When to use**
     *
     * Use to verify that arbitrary values generated for this schema satisfy the
     * schema's predicate.
     *
     * **Details**
     *
     * `verifyGeneration()` generates arbitrary values and asserts each value satisfies the schema's `is` predicate. It
     * defaults to 20 runs. Native checking options can control generation, shrinking, bounded discards, and replay.
     *
     * **Example** (Verifying arbitrary generation)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const asserts = new TestSchema.Asserts(Schema.String)
     * asserts.arbitrary().verifyGeneration({ seed: 1, runs: 20 }) // => undefined
     * ```
     *
     * @see {@link verifyRoundTrip} for property-based round-trip checks
     * @stability unstable
     */
    arbitrary<S extends Schema.ConstraintCodec<unknown, unknown>>(this: Asserts<S>): {
        /**
         * Asserts that generated values satisfy the schema's predicate.
         *
         * @stability unstable
         */
        verifyGeneration(options?: Arbitrary.CheckOptions): void;
    };
}
/**
 * Provides decoding test assertions through `succeed` and `fail` methods that run the schema's decoder and compare the result.
 *
 * **When to use**
 *
 * Use when you want to assert that specific inputs decode to expected values, invalid inputs produce specific error messages, or schemas receive required decoding services.
 *
 * **Details**
 *
 * `succeed` and `fail` return Promises. `succeedEffect` and `failEffect` return lazy
 * effects that preserve required decoding services. All assertions use
 * `assert.deepStrictEqual` internally. With one argument, success assertions
 * compare the output to the input; with two, they compare it to `expected`.
 * Failure assertions compare the formatted schema issue to `message`.
 * `provide(key, impl)` returns a new `Decoding` with the service injected.
 *
 * **Example** (Decoding with service provision)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { TestSchema } from "effect/testing"
 *
 * const asserts = new TestSchema.Asserts(Schema.String)
 * const decoding = asserts.decoding()
 * await decoding.succeed("hello") // => undefined
 * ```
 *
 * @see {@link Asserts}
 * @see {@link Encoding}
 * @stability unstable
 * @category testing
 * @since 4.0.0
 */
export declare class Decoding<S extends Schema.Constraint> {
    /**
     * Schema used by these decoding assertions.
     *
     * @stability unstable
     */
    readonly schema: S;
    /**
     * Decoder used by these assertions, preserving schema issues and required services.
     *
     * @stability unstable
     */
    readonly decodeUnknownEffect: (input: unknown, options?: SchemaAST.ParseOptions) => Effect.Effect<S["Type"], SchemaIssue.Issue, S["DecodingServices"]>;
    /**
     * Parsing options used by these assertions.
     *
     * @stability unstable
     */
    readonly options?: {
        readonly parseOptions?: SchemaAST.ParseOptions | undefined;
    } | undefined;
    /**
     * Creates decoding assertions for a schema and optional parsing options.
     *
     * @stability unstable
     */
    constructor(schema: S, options?: {
        readonly parseOptions?: SchemaAST.ParseOptions | undefined;
    });
    /**
     * Asserts that decoding `input` succeeds. With one argument, asserts the
     * output equals the input. With two arguments, asserts the output equals
     * `expected`.
     *
     * **When to use**
     *
     * Use to verify successful decoding for one input case.
     *
     * **Example** (Testing identity and transformed decoding)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const decoding = new TestSchema.Asserts(Schema.NumberFromString).decoding()
     * await decoding.succeed("1", 1) // => undefined
     * ```
     *
     * @see {@link succeedEffect} for the Effect variant
     * @see {@link fail} for asserting decoding failures
     * @stability unstable
     */
    succeed<S extends Schema.ConstraintDecoder<unknown, never>>(this: Decoding<S>, input: unknown): Promise<void>;
    succeed<S extends Schema.ConstraintDecoder<unknown, never>>(this: Decoding<S>, input: unknown, expected: S["Type"]): Promise<void>;
    /**
     * Returns a lazy assertion that decoding succeeds with the input or the
     * explicit expected value.
     *
     * **When to use**
     *
     * Use to assert decoding in an Effect test while preserving required decoding services.
     *
     * **Details**
     *
     * Assertion mismatches are defects. The decoder uses the calling fiber's services.
     *
     * @see {@link succeed} for the Promise variant
     * @see {@link failEffect} for asserting decoding failures
     * @stability unstable
     */
    succeedEffect(input: unknown): Effect.Effect<void, never, S["DecodingServices"]>;
    succeedEffect(input: unknown, expected: S["Type"]): Effect.Effect<void, never, S["DecodingServices"]>;
    /**
     * Asserts that decoding `input` fails and the stringified issue equals
     * `message`.
     *
     * **When to use**
     *
     * Use to verify that invalid decoding input produces the expected issue text.
     *
     * **Example** (Asserting a decoding failure)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const decoding = new TestSchema.Asserts(Schema.String).decoding()
     * await decoding.fail(42, "Expected string") // => undefined
     * ```
     *
     * @see {@link failEffect} for the Effect variant
     * @see {@link succeed} for asserting successful decoding
     * @stability unstable
     */
    fail<S extends Schema.ConstraintDecoder<unknown, never>>(this: Decoding<S>, input: unknown, message: string): Promise<void>;
    /**
     * Returns a lazy assertion that decoding fails with the expected issue message.
     *
     * **Details**
     *
     * Assertion mismatches are defects. Schema defects and interruption propagate
     * without satisfying the assertion. Required decoding services come from the
     * calling fiber.
     *
     * @see {@link fail} for the Promise variant
     * @see {@link succeedEffect} for asserting decoding success
     * @stability unstable
     */
    failEffect(input: unknown, message: string): Effect.Effect<void, never, S["DecodingServices"]>;
    /**
     * Returns a new {@link Decoding} instance with the given service injected into the decoding effect context.
     *
     * **When to use**
     *
     * Use when the schema's decoder requires a service dependency.
     *
     * @see {@link Encoding.provide}
     * @stability unstable
     */
    provide<Id, Service>(service: Context.Key<Id, Service>, implementation: Service): Decoding<Schema.middlewareDecoding<S, Exclude<S["DecodingServices"], Id>>>;
}
/**
 * Provides encoding test assertions through `succeed` and `fail` methods that run the schema's encoder and compare the result.
 *
 * **When to use**
 *
 * Use when you want to assert that specific values encode to expected outputs, invalid inputs produce specific error messages, or schemas receive required encoding services.
 *
 * **Details**
 *
 * `succeed` and `fail` return Promises. `succeedEffect` and `failEffect` return lazy
 * effects that preserve required encoding services. All assertions use
 * `assert.deepStrictEqual` internally. With one argument, success assertions
 * compare the output to the input; with two, they compare it to `expected`.
 * Failure assertions compare the formatted schema issue to `message`.
 * `provide(key, impl)` returns a new `Encoding` with the service injected.
 *
 * **Example** (Encoding assertions)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { TestSchema } from "effect/testing"
 *
 * const encoding = new TestSchema.Asserts(Schema.NumberFromString).encoding()
 * await encoding.succeed(42, "42") // => undefined
 * ```
 *
 * @see {@link Asserts}
 * @see {@link Decoding}
 *
 * @stability unstable
 * @category testing
 * @since 4.0.0
 */
export declare class Encoding<S extends Schema.Constraint> {
    /**
     * Schema used by these encoding assertions.
     *
     * @stability unstable
     */
    readonly schema: S;
    /**
     * Encoder used by these assertions, preserving schema issues and required services.
     *
     * @stability unstable
     */
    readonly encodeUnknownEffect: (input: unknown, options?: SchemaAST.ParseOptions) => Effect.Effect<S["Encoded"], SchemaIssue.Issue, S["EncodingServices"]>;
    /**
     * Parsing options used by these assertions.
     *
     * @stability unstable
     */
    readonly options?: {
        readonly parseOptions?: SchemaAST.ParseOptions | undefined;
    } | undefined;
    /**
     * Creates encoding assertions for a schema and optional parsing options.
     *
     * @stability unstable
     */
    constructor(schema: S, options?: {
        readonly parseOptions?: SchemaAST.ParseOptions | undefined;
    });
    /**
     * Asserts that encoding `input` succeeds. With one argument, asserts the
     * output equals the input. With two arguments, asserts the output equals
     * `expected`.
     *
     * **When to use**
     *
     * Use to verify successful encoding for one input case.
     *
     * **Example** (Testing identity and transformed encoding)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const encoding = new TestSchema.Asserts(Schema.NumberFromString).encoding()
     * await encoding.succeed(1, "1") // => undefined
     * ```
     *
     * @see {@link succeedEffect} for the Effect variant
     * @see {@link fail} for asserting encoding failures
     * @stability unstable
     */
    succeed<S extends Schema.ConstraintEncoder<unknown, never>>(this: Encoding<S>, input: unknown): Promise<void>;
    succeed<S extends Schema.ConstraintEncoder<unknown, never>>(this: Encoding<S>, input: unknown, expected: S["Encoded"]): Promise<void>;
    /**
     * Returns a lazy assertion that encoding succeeds with the input or the
     * explicit expected value.
     *
     * **When to use**
     *
     * Use to assert encoding in an Effect test while preserving required encoding services.
     *
     * **Details**
     *
     * Assertion mismatches are defects. The encoder uses the calling fiber's services.
     *
     * @see {@link succeed} for the Promise variant
     * @see {@link failEffect} for asserting encoding failures
     * @stability unstable
     */
    succeedEffect(input: unknown): Effect.Effect<void, never, S["EncodingServices"]>;
    succeedEffect(input: unknown, expected: S["Encoded"]): Effect.Effect<void, never, S["EncodingServices"]>;
    /**
     * Asserts that encoding `input` fails and the stringified issue equals
     * `message`.
     *
     * **When to use**
     *
     * Use to verify that invalid encoding input produces the expected issue text.
     *
     * **Example** (Asserting an encoding failure)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { TestSchema } from "effect/testing"
     *
     * const encoding = new TestSchema.Asserts(Schema.NumberFromString).encoding()
     * await encoding.fail("not-a-number", "Expected number") // => undefined
     * ```
     *
     * @see {@link failEffect} for the Effect variant
     * @see {@link succeed} for asserting successful encoding
     * @stability unstable
     */
    fail<S extends Schema.ConstraintEncoder<unknown, never>>(this: Encoding<S>, input: unknown, message: string): Promise<void>;
    /**
     * Returns a lazy assertion that encoding fails with the expected issue message.
     *
     * **Details**
     *
     * Assertion mismatches are defects. Schema defects and interruption propagate
     * without satisfying the assertion. Required encoding services come from the
     * calling fiber.
     *
     * @see {@link fail} for the Promise variant
     * @see {@link succeedEffect} for asserting encoding success
     * @stability unstable
     */
    failEffect(input: unknown, message: string): Effect.Effect<void, never, S["EncodingServices"]>;
    /**
     * Returns a new {@link Encoding} instance with the given service injected into the encoding effect context.
     *
     * **When to use**
     *
     * Use when the schema's encoder requires a service dependency.
     *
     * @see {@link Decoding.provide}
     * @stability unstable
     */
    provide<Id, Service>(service: Context.Key<Id, Service>, implementation: Service): Encoding<Schema.middlewareEncoding<S, Exclude<S["EncodingServices"], Id>>>;
}
//# sourceMappingURL=TestSchema.d.ts.map