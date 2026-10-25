/**
 * Installs runtime-generated schema decoders without changing SchemaParser's
 * public interface. Operations are compiled lazily. Import the separate
 * `SchemaJITCompiler/enable` module to enable compilation globally.
 *
 * @stability unstable
 * @since 4.0.0
 */
import { generate, shouldCompileParser } from "../internal/schema/codegen.js";
import * as Registry from "../internal/schema/compilerRegistry.js";
import { runtime } from "./SchemaCompiler/runtime.js";
let checked;
let supported = false;
/**
 * @internal
 */
export const compiler = (ast, resolve) => {
  if (!shouldCompileParser(ast)) return undefined;
  if (checked !== globalThis.Function) {
    checked = globalThis.Function;
    try {
      checked("return true");
      supported = true;
    } catch {
      supported = false;
    }
  }
  if (!supported) return undefined;
  let decodeFailed = false;
  const operation = key => {
    const isConstruction = key === "make" || key === "makeEffect";
    if (isConstruction || !decodeFailed) {
      try {
        const source = generate(ast, key);
        if (source === undefined) return undefined;
        return globalThis.Function("ast", "R", "resolve", source)(ast, runtime, resolve);
      } catch {
        // Only code generation and initialization are inside this catch.
        if (!isConstruction) decodeFailed = true;
      }
    }
    return key === "decodeEffect" ? runtime.decode(ast, resolve) : key === "makeEffect" ? runtime.make(ast, resolve) : undefined;
  };
  return {
    get is() {
      return operation("is");
    },
    get decode() {
      return operation("decode");
    },
    get make() {
      return operation("make");
    },
    get decodeEffect() {
      return operation("decodeEffect");
    },
    get makeEffect() {
      return operation("makeEffect");
    }
  };
};
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
export function enable(ast) {
  Registry.enable(ast, compiler);
}
//# sourceMappingURL=SchemaJITCompiler.js.map