/**
 * Provides helpers for testing Schema behavior.
 *
 * These utilities assert how schemas construct values, decode input, encode
 * output, generate arbitrary values, and round-trip between encoded and decoded
 * forms. The `Asserts` class groups the common checks for one schema, while
 * `Decoding` and `Encoding` can be used directly when a test only needs one
 * direction.
 *
 * Methods with an `Effect` suffix return lazy assertions that use the calling
 * fiber's services. The Promise-returning methods can be used with any test
 * runner.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as assert from "node:assert";
import { isDeepStrictEqual } from "node:util";
import * as Arbitrary from "../Arbitrary.js";
import * as Cause from "../Cause.js";
import * as Effect from "../Effect.js";
import * as Exit from "../Exit.js";
import { pipe } from "../Function.js";
import * as InternalSchemaCause from "../internal/schema/cause.js";
import * as Result from "../Result.js";
import * as Schema from "../Schema.js";
import * as SchemaAST from "../SchemaAST.js";
import * as SchemaIssue from "../SchemaIssue.js";
import * as SchemaParser from "../SchemaParser.js";
function assertPropertyPassed(result) {
  const failure = Arbitrary.formatCheckFailure(result);
  if (failure !== undefined) assert.fail(failure);
}
const schemaResult = /*#__PURE__*/Effect.fnUntraced(function* (effect) {
  const exit = yield* Effect.exit(effect);
  if (Exit.isSuccess(exit)) {
    return Result.succeed(exit.value);
  }
  const issue = InternalSchemaCause.getSchemaIssue(exit.cause);
  if (issue !== undefined) {
    return Result.fail(SchemaIssue.defaultFormatter(issue));
  }
  return yield* Effect.failCause(Cause.fromReasons(exit.cause.reasons.map(reason => Cause.isFailReason(reason) ? Cause.makeDieReason(reason.error).annotate(Cause.reasonAnnotations(reason)) : reason)));
});
const assertResult = /*#__PURE__*/Effect.fnUntraced(function* (parse, expected) {
  const result = yield* schemaResult(parse());
  assert.deepStrictEqual(result, expected);
});
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
export class Asserts {
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
  static ast = {
    /**
     * Groups schema field comparisons.
     *
     * @stability unstable
     */
    fields: {
      /**
       * Asserts that schema fields have equal AST structures.
       *
       * @stability unstable
       */
      equals: (a, b) => {
        assert.deepStrictEqual(Object.fromEntries(Reflect.ownKeys(a).map(key => [key, SchemaAST.getAST(a[key])])), Object.fromEntries(Reflect.ownKeys(b).map(key => [key, SchemaAST.getAST(b[key])])));
      }
    },
    /**
     * Groups tuple element comparisons.
     *
     * @stability unstable
     */
    elements: {
      /**
       * Asserts that tuple elements have equal AST structures.
       *
       * @stability unstable
       */
      equals: (a, b) => {
        assert.deepStrictEqual(a.map(SchemaAST.getAST), b.map(SchemaAST.getAST));
      }
    }
  };
  /**
   * Schema used by these assertions.
   *
   * @stability unstable
   */
  schema;
  /**
   * Creates assertions for a schema.
   *
   * @stability unstable
   */
  constructor(schema) {
    this.schema = schema;
  }
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
  make(options) {
    const makeEffect = SchemaParser.makeEffect(this.schema);
    function succeedEffect(input, expected) {
      return assertResult(() => makeEffect(input, options), Result.succeed(arguments.length === 1 ? input : expected));
    }
    async function succeed(input, expected) {
      return Effect.runPromise(succeedEffect(input, arguments.length === 1 ? input : expected));
    }
    function failEffect(input, message) {
      return assertResult(() => makeEffect(input, options), Result.fail(message));
    }
    return {
      /**
       * Asserts that construction succeeds with the input or the explicit expected value.
       *
       * @stability unstable
       */
      succeed,
      /**
       * Returns a lazy construction success assertion whose mismatches are defects.
       *
       * @stability unstable
       */
      succeedEffect,
      /**
       * Returns a lazy assertion that construction fails with the expected issue message.
       *
       * @stability unstable
       */
      failEffect,
      /**
       * Asserts that construction fails with the expected issue message.
       *
       * @stability unstable
       */
      async fail(input, message) {
        return Effect.runPromise(failEffect(input, message));
      }
    };
  }
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
  verifyRoundTrip(options) {
    return Effect.runPromise(this.verifyRoundTripEffect(options));
  }
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
  verifyRoundTripEffect(options) {
    return Effect.suspend(() => {
      const decodeUnknownEffect = SchemaParser.decodeUnknownEffect(this.schema);
      const encodeEffect = SchemaParser.encodeEffect(this.schema);
      const arbitrary = Arbitrary.schema(this.schema);
      return Arbitrary.checkEffect(arbitrary, value => Effect.suspend(() => encodeEffect(value)).pipe(Effect.flatMapEager(encoded => decodeUnknownEffect(encoded)), schemaResult, Effect.mapEager(result => isDeepStrictEqual(result, Result.succeed(value)))), options).pipe(Effect.mapEager(assertPropertyPassed));
    });
  }
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
  decoding(options) {
    return new Decoding(this.schema, options);
  }
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
  encoding(options) {
    return new Encoding(this.schema, options);
  }
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
  arbitrary() {
    const schema = this.schema;
    return {
      /**
       * Asserts that generated values satisfy the schema's predicate.
       *
       * @stability unstable
       */
      verifyGeneration(options) {
        const is = Schema.is(schema);
        const arbitrary = Arbitrary.schema(schema);
        assertPropertyPassed(Effect.runSync(Arbitrary.checkEffect(arbitrary, is, {
          runs: 20,
          ...options
        })));
      }
    };
  }
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
export class Decoding {
  /**
   * Schema used by these decoding assertions.
   *
   * @stability unstable
   */
  schema;
  /**
   * Decoder used by these assertions, preserving schema issues and required services.
   *
   * @stability unstable
   */
  decodeUnknownEffect;
  /**
   * Parsing options used by these assertions.
   *
   * @stability unstable
   */
  options;
  /**
   * Creates decoding assertions for a schema and optional parsing options.
   *
   * @stability unstable
   */
  constructor(schema, options) {
    this.schema = schema;
    this.decodeUnknownEffect = SchemaParser.decodeUnknownEffect(schema);
    this.options = options;
  }
  async succeed(input, expected) {
    return Effect.runPromise(this.succeedEffect(input, arguments.length === 1 ? input : expected));
  }
  succeedEffect(input, expected) {
    return assertResult(() => this.decodeUnknownEffect(input, this.options?.parseOptions), Result.succeed(arguments.length === 1 ? input : expected));
  }
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
  async fail(input, message) {
    return Effect.runPromise(this.failEffect(input, message));
  }
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
  failEffect(input, message) {
    return assertResult(() => this.decodeUnknownEffect(input, this.options?.parseOptions), Result.fail(message));
  }
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
  provide(service, implementation) {
    return new Decoding(pipe(this.schema, Schema.middlewareDecoding(Effect.provideService(service, implementation))), this.options);
  }
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
export class Encoding {
  /**
   * Schema used by these encoding assertions.
   *
   * @stability unstable
   */
  schema;
  /**
   * Encoder used by these assertions, preserving schema issues and required services.
   *
   * @stability unstable
   */
  encodeUnknownEffect;
  /**
   * Parsing options used by these assertions.
   *
   * @stability unstable
   */
  options;
  /**
   * Creates encoding assertions for a schema and optional parsing options.
   *
   * @stability unstable
   */
  constructor(schema, options) {
    this.schema = schema;
    this.encodeUnknownEffect = SchemaParser.encodeUnknownEffect(schema);
    this.options = options;
  }
  async succeed(input, expected) {
    return Effect.runPromise(this.succeedEffect(input, arguments.length === 1 ? input : expected));
  }
  succeedEffect(input, expected) {
    return assertResult(() => this.encodeUnknownEffect(input, this.options?.parseOptions), Result.succeed(arguments.length === 1 ? input : expected));
  }
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
  async fail(input, message) {
    return Effect.runPromise(this.failEffect(input, message));
  }
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
  failEffect(input, message) {
    return assertResult(() => this.encodeUnknownEffect(input, this.options?.parseOptions), Result.fail(message));
  }
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
  provide(service, implementation) {
    return new Encoding(pipe(this.schema, Schema.middlewareEncoding(Effect.provideService(service, implementation))), this.options);
  }
}
//# sourceMappingURL=TestSchema.js.map