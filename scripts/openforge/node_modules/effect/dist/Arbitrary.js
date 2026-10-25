/**
 * Derives, samples, and checks generated values from Effect Schema.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Cause from "./Cause.js";
import * as Formatter from "./Formatter.js";
import { dual } from "./Function.js";
import * as Internal from "./internal/arbitrary/runner.js";
import { hasProperty } from "./Predicate.js";
/**
 * Runtime type identifier for `Arbitrary` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TypeId = Internal.TypeId;
/**
 * Checks whether a value is an `Arbitrary`.
 *
 * **When to use**
 *
 * Use when accepting both Arbitrary values and other input descriptions.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isArbitrary = u => hasProperty(u, TypeId);
/**
 * Replaces the defaults used by {@link checkEffect} and {@link sampleEffect}.
 *
 * **Details**
 *
 * Options are resolved once per execution: explicit non-`undefined` values, then configured defaults, then built-in
 * defaults. The supplied configuration is copied. Pass `{}` to reset it. Replay tokens retain control of replayed checks.
 *
 * **Gotchas**
 *
 * Configure defaults before starting concurrent tests. Changes affect subsequent executions, including Effects created
 * earlier, but do not affect active runs.
 *
 * @stability unstable
 * @category configuration
 * @since 4.0.0
 */
export const configureGlobal = Internal.configureGlobal;
/**
 * Formats an unsuccessful property-check result as a diagnostic message, returning `undefined` for a passed result.
 *
 * **When to use**
 *
 * Use when integrating `checkEffect` with a test runner or another reporting interface.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export function formatCheckFailure(result) {
  switch (result._tag) {
    case "Passed":
      return undefined;
    case "Falsified":
      return `Property falsified after ${result.runs} run(s) and ${result.shrinks} shrink(s)\n` + `Shrunk input: ${Formatter.format(result.shrunkInput, {
        space: 2
      })}\n` + `${result.failure._tag === "ReturnedFalse" ? "Failure: returned false" : `Failure: ${Cause.isCause(result.failure.error) ? Cause.pretty(result.failure.error) : Formatter.format(result.failure.error, {
        space: 2
      })}`}\n` + `Replay: ${result.replay}`;
    case "Exhausted":
      return `Property exhausted after ${result.runs} run(s) and ${result.discards} discard(s)\n` + `Seed: ${Formatter.format(result.seed, {
        space: 2
      })}`;
    case "ReplayMismatch":
      return `Property replay failed: ${result.reason}`;
  }
}
/**
 * Derives an `Arbitrary` from the decoded `Type` of a Schema.
 *
 * **When to use**
 *
 * Use when you want Schema-aware generation without exposing a third-party property-testing engine.
 *
 * **Details**
 *
 * When `options.shrink` is provided, generated roots still come from Schema derivation, while the callback defines the
 * complete shrink tree. Invalid candidates are skipped and count against `maxShrinks` without reaching the property.
 *
 * **Gotchas**
 *
 * Derivation is immediate and throws when the current unstable implementation cannot compile the Schema or prove a
 * finite route through a recursive component.
 *
 * A custom shrinker replaces Schema-derived shrinking. It is evaluated lazily after a property failure and must be
 * synchronous, deterministic, terminating, and free of mutation.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export function schema(schema, options) {
  return Internal.schema(schema, options);
}
/**
 * Creates an `Arbitrary` that always generates `value` and has no shrink candidates.
 *
 * **When to use**
 *
 * Use when a branch of dependent generation should produce an already constructed value.
 *
 * **Gotchas**
 *
 * Every generation returns the same value. Objects are not cloned, so properties must not mutate them.
 *
 * @see {@link flatMap} for selecting dependent Arbitraries
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export function Constant(value) {
  return Internal.constant(value);
}
/**
 * Generates variable-length arrays from an existing element `Arbitrary`.
 *
 * **When to use**
 *
 * Use when generating command sequences or collections whose elements are built with Arbitrary composition.
 *
 * **Details**
 *
 * Shrinking tries removing blocks, including prefixes and interior blocks, while preserving the order and values of
 * retained elements. It also tries the elements' own shrink candidates. Length bounds remain valid during shrinking,
 * and elements share the generation budget for recursion.
 *
 * **Gotchas**
 *
 * Invalid length bounds throw a `RangeError` when this function is called. Retained objects are not cloned, so
 * properties must not mutate generated values. Bounded shrinking does not guarantee a globally minimal failure.
 *
 * **Example** (Removing irrelevant commands)
 *
 * ```ts import.meta.vitest
 * import { Arbitrary, Effect, Schema } from "effect"
 *
 * const command = Arbitrary.schema(Schema.Literals(["Add", "Reset", "Stop"]))
 * const commands = Arbitrary.array(command, { maxLength: 50 })
 * const result = await Effect.runPromise(
 *   Arbitrary.checkEffect(commands, (values) => {
 *     // The state machine fails when Reset occurs before a later Stop.
 *     const reset = values.indexOf("Reset")
 *     return reset === -1 || !values.slice(reset + 1).includes("Stop")
 *   }, { runs: 1, size: 4, seed: 0 })
 * )
 *
 * result._tag // => "Falsified"
 * if (result._tag === "Falsified") {
 *   result.initialInput // => ["Add", "Add", "Reset", "Stop"]
 *   result.shrunkInput // => ["Reset", "Stop"]
 * }
 * ```
 *
 * @see {@link ArrayOptions} for length bounds
 * @see {@link all} for fixed tuples and records of Arbitraries
 * @see {@link schema} for deriving arrays described by Schema
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const array = Internal.array;
/**
 * Transforms every generated value and its shrink candidates.
 *
 * **When to use**
 *
 * Use when you want to derive generated values from an existing `Arbitrary` without changing its generation or shrink
 * structure.
 *
 * @stability unstable
 * @category mapping
 * @since 4.0.0
 */
export const map = /*#__PURE__*/dual(2, (self, f) => Internal.map(self, f));
/**
 * Keeps generated values and shrink candidates that satisfy a predicate or refinement.
 *
 * **When to use**
 *
 * Use when a condition cannot be expressed constructively by the source Schema or after values have been transformed.
 *
 * **Gotchas**
 *
 * Rejected generated values count against `maxDiscards`. Prefer Schema checks when possible because the Schema compiler
 * may generate matching values directly.
 *
 * @see {@link filterMap} for transforming and filtering simultaneously
 * @stability unstable
 * @category filtering
 * @since 4.0.0
 */
export const filter = /*#__PURE__*/dual(2, (self, predicate) => Internal.filter(self, predicate));
/**
 * Transforms accepted generated values and discards rejected values.
 *
 * **When to use**
 *
 * Use when transformation and validation need to happen in one step after constructing an `Arbitrary`.
 *
 * **Gotchas**
 *
 * Failed filters discard generated roots and count against `maxDiscards`. Failures are not exposed in sampling or
 * checking results.
 *
 * @see {@link map} for transformations that cannot reject
 * @see {@link filter} for retaining original values that satisfy a condition
 * @stability unstable
 * @category filtering
 * @since 4.0.0
 */
export const filterMap = /*#__PURE__*/dual(2, (self, f) => Internal.filterMap(self, f));
/**
 * Sequentially selects an `Arbitrary` from a generated value.
 *
 * **When to use**
 *
 * Use when the domain or shape of a generated value depends on another generated value.
 *
 * **Details**
 *
 * Shrinking first tries smaller source values and regenerates their dependent Arbitraries. It then shrinks the
 * selected dependent value. After a dependent shrink is selected, source shrinking is closed for that branch.
 *
 * **Gotchas**
 *
 * The callback must be synchronous, deterministic, and terminating. It can be evaluated again during shrinking and
 * replay. Deriving a Schema inside the callback also repeats that derivation, so precompile finite dependent
 * Arbitraries when possible.
 *
 * @see {@link map} for total transformations that do not select another Arbitrary
 * @see {@link Constant} for dependent branches that return an existing value
 * @stability unstable
 * @category sequencing
 * @since 4.0.0
 */
export const flatMap = /*#__PURE__*/dual(2, (self, f) => Internal.flatMap(self, f));
/**
 * Combines Arbitraries into one `Arbitrary` whose generated value mirrors the input shape.
 *
 * **When to use**
 *
 * Use when you need to generate several independent values together.
 *
 * **Details**
 *
 * Accepts a tuple or array, an iterable, or a record of Arbitraries. Tuple positions and record keys are preserved in
 * the generated value. Members are generated in a randomized internal order so recursive members share the generation
 * budget fairly, while shrinking changes one member at a time.
 *
 * **Gotchas**
 *
 * Iterable inputs are consumed when `all` is called. If any member discards a generated root, the complete generated
 * value is discarded.
 *
 * @see {@link array} for variable-length arrays that shrink by removing elements
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export function all(input) {
  return Internal.all(input);
}
/**
 * Generates a bounded collection of values from an `Arbitrary`.
 *
 * **When to use**
 *
 * Use when you need generated examples without running a property.
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export function sampleEffect(self, options) {
  return Internal.sampleEffect(self, options);
}
/**
 * Checks a pure or effectful property and shrinks the first falsification.
 *
 * **When to use**
 *
 * Use when you want deterministic, interruptible property checking with typed property failures and replay.
 *
 * **Details**
 *
 * Returning `false` and failing an Effect are shrinkable falsifications. Shrinking preserves which of these two
 * failure classes caused the initial falsification. Typed error values may change while shrinking and are not compared
 * for equality. Defects and interruption continue through the returned Effect instead of becoming `CheckResult`
 * values.
 *
 * **Gotchas**
 *
 * Properties must treat generated values as immutable. The runner does not clone values before evaluation, so
 * mutation can change reported shrunk inputs or interfere with shrinking and replay.
 *
 * A property must also produce the same outcome for the same input and initial environment. The runner may evaluate
 * it repeatedly and does not restore mutable services between evaluations. Stateful properties should acquire and
 * release an independent fixture inside each evaluation.
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export function checkEffect(self, property, options) {
  return Internal.checkEffect(self, property, options);
}
//# sourceMappingURL=Arbitrary.js.map