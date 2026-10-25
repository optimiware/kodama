import type * as SchemaAST from "../SchemaAST.ts";
/**
 * Enables lazy JIT compilation for an AST and its parsing dependencies.
 *
 * **Details**
 *
 * Uses the same registry as `SchemaCompiler.set`. Compilation failures, including
 * environments that block `new Function`, retain interpreted parsing. Errors
 * thrown while parsing are not retried. No checks, transformations or defaults
 * execute during installation.
 *
 * **Gotchas**
 *
 * Enable before first use to optimize every consumer. Functions that already
 * captured an entry keep it. Enable type-side and flipped ASTs separately when
 * they differ from the supplied AST.
 *
 * @stability unstable
 * @category compilation
 * @since 4.0.0
 */
export declare function enable(ast: SchemaAST.AST): void;
//# sourceMappingURL=SchemaJITCompiler.d.ts.map