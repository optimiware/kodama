/**
 * Builds self-installing modules from ahead-of-time compiled Schema decoders.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Data from "../../Data.js";
import * as Effect from "../../Effect.js";
import * as FileSystem from "../../FileSystem.js";
import * as Path from "../../Path.js";
import * as Schema from "../../Schema.js";
import * as SchemaAST from "../../SchemaAST.js";
import * as SchemaAOTCompiler from "../SchemaAOTCompiler.js";
/**
 * An error raised while loading modules or generating an AOT module.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class BuildError extends /*#__PURE__*/Data.TaggedError("BuildError") {}
const operationOrder = ["decode", "encode", "is", "make"];
const importPath = (path, outFile, baseUrl, specifier) => Effect.gen(function* () {
  const url = yield* Effect.try({
    try: () => new URL(specifier, baseUrl),
    catch: cause => new BuildError({
      cause,
      kind: "ResolveModule",
      message: `Could not resolve schema module ${JSON.stringify(specifier)}`,
      module: specifier
    })
  });
  const sourcePath = yield* path.fromFileUrl(url).pipe(Effect.mapError(cause => new BuildError({
    cause,
    kind: "ResolveModule",
    message: `Schema module ${JSON.stringify(specifier)} does not resolve to a file`,
    module: specifier
  })));
  const relative = path.relative(path.dirname(outFile), sourcePath);
  if (path.isAbsolute(relative)) {
    return yield* new BuildError({
      cause: undefined,
      kind: "ResolveModule",
      message: `Schema module ${JSON.stringify(specifier)} cannot be imported relative to the output file`,
      module: specifier
    });
  }
  const normalized = path.sep === "/" ? relative : relative.split(path.sep).join("/");
  return normalized.startsWith(".") ? normalized : `./${normalized}`;
});
const root = (exported, operation) => {
  const expression = `${exported.alias}[${JSON.stringify(exported.exportName)}].ast`;
  switch (operation) {
    case "decode":
      return [exported.schema.ast, "decode", expression, false];
    case "encode":
      {
        const ast = SchemaAST.flip(exported.schema.ast);
        return ast === exported.schema.ast ? [ast, "decode", expression, false] : [ast, "decode", `A.flip(${expression})`, true];
      }
    case "is":
    case "make":
      {
        const ast = SchemaAST.toType(exported.schema.ast);
        return ast === exported.schema.ast ? [ast, operation, expression, false] : [ast, operation, `A.toType(${expression})`, true];
      }
  }
};
/**
 * Writes a self-installing AOT module for Schema values exported by a set of
 * modules.
 *
 * **When to use**
 *
 * Use when a build script should let an application install generated decoders
 * by importing one generated module at startup.
 *
 * **Details**
 *
 * Module keys are import specifiers relative to `baseUrl`. Their loaders run
 * sequentially during the build. The builder sorts module specifiers and export
 * names, compiles direct Schema exports, and writes deterministic source using
 * `FileSystem`. The generated module imports those exports and installs their
 * decoders in the shared Schema parser registry as a module side effect. A lazy
 * record returned by `import.meta.glob` can be passed as `modules` directly.
 *
 * Decoding is prepared by default. `is` and `make` prepare the type-side AST;
 * `encode` prepares the flipped AST. Nested dependencies are discovered by the
 * AOT compiler and do not need separate exports.
 *
 * **Gotchas**
 *
 * Loaders execute application modules during the build. Each loader must return
 * the same module named by its key. The generated file must be rebuilt after a
 * schema definition or Effect version changes. Unsupported operations retain
 * the interpreter fallback. Build configurations that mark modules as
 * side-effect free must retain the generated import.
 *
 * @stability unstable
 * @category compilation
 * @since 4.0.0
 */
export const build = /*#__PURE__*/Effect.fnUntraced(function* (options) {
  const fs = yield* FileSystem.FileSystem;
  const path = yield* Path.Path;
  const outFile = path.resolve(options.outFile);
  const requested = new Set(options.operations ?? ["decode"]);
  const operations = operationOrder.filter(operation => requested.has(operation));
  const imports = [];
  const exportedSchemas = [];
  for (const specifier of Object.keys(options.modules).sort()) {
    const loader = options.modules[specifier];
    const loaded = yield* Effect.tryPromise({
      try: () => loader(),
      catch: cause => new BuildError({
        cause,
        kind: "LoadModule",
        message: `Could not load schema module ${JSON.stringify(specifier)}`,
        module: specifier
      })
    });
    if (typeof loaded !== "object" || loaded === null) {
      return yield* new BuildError({
        cause: loaded,
        kind: "InvalidModule",
        message: `Schema module ${JSON.stringify(specifier)} did not load a module namespace`,
        module: specifier
      });
    }
    const namespace = loaded;
    const exports = Object.keys(namespace).filter(name => Schema.isSchema(namespace[name])).sort();
    if (exports.length === 0) continue;
    const alias = `m${imports.length}`;
    const specifierFromOutput = yield* importPath(path, outFile, options.baseUrl, specifier);
    imports.push(`import * as ${alias} from ${JSON.stringify(specifierFromOutput)};`);
    for (const exportName of exports) {
      exportedSchemas.push({
        alias,
        exportName,
        schema: namespace[exportName]
      });
    }
  }
  const source = yield* Effect.try({
    try: () => {
      const targets = [];
      const seen = new Map();
      let needsSchemaASTImport = false;
      for (const exported of exportedSchemas) {
        for (const operation of operations) {
          const [ast, targetOperation, expression, isDerived] = root(exported, operation);
          const index = seen.get(ast);
          if (index !== undefined) {
            const targetOperations = targets[index].operations;
            if (!targetOperations.includes(targetOperation)) targetOperations.push(targetOperation);
            continue;
          }
          seen.set(ast, targets.length);
          targets.push({
            ast,
            operations: [targetOperation],
            expression
          });
          needsSchemaASTImport ||= isDerived;
        }
      }
      return [...imports, ...(needsSchemaASTImport ? ["import * as A from \"effect/SchemaAST\";"] : []), SchemaAOTCompiler.compile(targets), `install([${targets.map(target => target.expression).join(",")}]);`, ""].join("\n");
    },
    catch: cause => new BuildError({
      cause,
      kind: "Generate",
      message: "Could not generate the Schema AOT module"
    })
  });
  yield* fs.makeDirectory(path.dirname(outFile), {
    recursive: true
  });
  yield* fs.writeFileString(outFile, source);
  return {
    outFile,
    modules: imports.length,
    schemas: exportedSchemas.length
  };
});
//# sourceMappingURL=Build.js.map