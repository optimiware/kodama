import * as SchemaAST from "../SchemaAST.ts";
/**
 * A parser operation prepared by {@link compile}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Operation = "decode" | "is" | "make";
/**
 * An exact AST and the parser operations to prepare for it.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Target {
    /** The registry key installed by the generated module. */
    readonly ast: SchemaAST.AST;
    /** The operations that should be compiled for this AST. */
    readonly operations: ReadonlyArray<Operation>;
}
/**
 * Generates a JavaScript ES module exporting `install(asts): void` for an
 * ordered array of compilation targets.
 *
 * **When to use**
 *
 * Use to prepare decoders at build time for environments that disallow dynamic
 * function construction. Save the returned source as a JavaScript module, then
 * call its `install` export with the target ASTs in the same order before using
 * parsers. Use a one-element array for a single schema.
 *
 * **Details**
 *
 * Generation does not install decoders or execute checks and transformations.
 * Installation uses the same registry as `SchemaCompiler.set`; normal
 * `SchemaParser` functions consume those entries. Generated validators and
 * Struct and homogeneous Array loops are static functions. They share diagnostic and
 * asynchronous continuation helpers with the interpreter. A single synchronous
 * transformation between supported leaf types can use generated orchestration.
 * Other detailed traversals, transformations, and middleware use the interpreter
 * with registry-resolved children. Transformations and middleware are not replayed.
 * Only the requested operation families and their static dependencies are
 * emitted. Installation registers roots and dependencies in the same registry;
 * each generated operation initializes on first use. Missing operations retain
 * the lazy interpreter fallback. Repeated ASTs and shared dependencies are
 * installed once by identity. Fast paths can still inline dependency code into
 * multiple parent decoders. An empty array generates a module whose installation
 * does nothing.
 * Construction uses independently lazy `make` and `makeEffect` operations.
 * Pure fixed Struct and homogeneous Array constructors can use `make` for a
 * synchronous fast path; failures delegate to `makeEffect` for detailed issues.
 * Tuple, Record, Union, leaf, and Class constructors use the existing
 * interpreter. Constructor defaults and Class source schemas are read from the
 * supplied ASTs, not serialized or executed during generation. `make` is
 * omitted whenever replay could repeat observable construction work.
 *
 * **Gotchas**
 *
 * Regenerate the module whenever the schema definition or Effect version
 * changes. Installation trusts that the runtime array has the same length and
 * target order, and its ASTs have the same definitions and sharing as at build
 * time. Functions and symbols are read from those ASTs, not serialized.
 * Suspend thunks are not evaluated during generation; their contents and other
 * unsupported nodes use the interpreter.
 * Installation replaces generated entries, but parsers that already captured
 * older entries keep them. Type-side and flipped ASTs are separate registry
 * keys; generate and install them separately when needed. Importing the
 * generated module alone does not install anything.
 * In particular, target `SchemaAST.toType(schema.ast)` with `make` to prepare
 * construction when it differs from the encoded root. Target
 * `SchemaAST.flip(schema.ast)` with `decode` to prepare encoding. Unsupported
 * operations use the interpreter while statically installed children remain
 * available.
 *
 * @stability unstable
 * @category compilation
 * @since 4.0.0
 */
export declare const compile: (targets: ReadonlyArray<Target>) => string;
//# sourceMappingURL=SchemaAOTCompiler.d.ts.map