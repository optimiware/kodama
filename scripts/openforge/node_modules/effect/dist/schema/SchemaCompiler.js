import * as CompilerRegistry from "../internal/schema/compilerRegistry.js";
import * as InternalParser from "../internal/schema/parser.js";
/**
 * The result returned by {@link Decode} or {@link Make} when the fast path fails.
 *
 * @stability unstable
 * @category symbols
 * @since 4.0.0
 */
export const invalid = CompilerRegistry.invalid;
/**
 * The sentinel distinguishing an absent input from a present `undefined`.
 * Decoders and constructors propagate it as a successful result when no value
 * is produced. Parents omit optional fields or report missing required keys;
 * public root adapters reject it rather than returning it to callers.
 *
 * @stability unstable
 * @category symbols
 * @since 4.0.0
 */
export const missing = InternalParser.missing;
/**
 * Installs a compiled decoder for an exact AST in the shared Schema parser
 * registry.
 *
 * **Details**
 *
 * A later call for the same AST replaces the previous entry. Parser functions
 * that have already resolved and retained an earlier entry are not updated.
 * This also applies when subsequent calls use different parse options.
 * The decoder is trusted to implement the semantics of the supplied AST.
 * Installation does not evaluate operation getters. Each operation, including
 * an absent optional operation, is resolved once when first needed. Accessors
 * retain the supplied decoder as their receiver. The supplied object is not
 * mutated. JIT installation uses these same rules.
 * Replacement includes construction: omitting `makeEffect` in the replacement
 * selects interpreted detailed construction for new consumers, without merging
 * the old operation into the new entry. An installed `make` can still handle
 * synchronous successes. Already captured constructors keep their entry.
 *
 * @stability unstable
 * @category registry
 * @since 4.0.0
 */
export const set = (ast, decoder) => {
  CompilerRegistry.set(ast, decoder);
};
//# sourceMappingURL=SchemaCompiler.js.map