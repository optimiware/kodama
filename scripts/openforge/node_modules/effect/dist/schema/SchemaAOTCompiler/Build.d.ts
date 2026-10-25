import * as Effect from "../../Effect.ts";
import * as FileSystem from "../../FileSystem.ts";
import * as Path from "../../Path.ts";
import type * as PlatformError from "../../PlatformError.ts";
/**
 * An operation whose root AST should be prepared by {@link build}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Operation = "decode" | "encode" | "is" | "make";
/**
 * Loads the exports of a schema module during a build.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface ModuleLoader {
    (): PromiseLike<unknown>;
}
/**
 * Options for {@link build}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface BuildOptions {
    /**
     * Maps import specifiers relative to `baseUrl` to loaders for those same
     * modules. Only directly exported Schema values are compiled.
     */
    readonly modules: Readonly<Record<string, ModuleLoader>>;
    /** The URL against which relative module specifiers are resolved. */
    readonly baseUrl: string | URL;
    /** The file-system path of the generated module. */
    readonly outFile: string;
    /** The parser operations to prepare. Defaults to decoding. */
    readonly operations?: ReadonlyArray<Operation> | undefined;
}
/**
 * A summary of a completed {@link build}.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface BuildResult {
    /** The absolute path written by the build. */
    readonly outFile: string;
    /** The number of loaded modules containing at least one Schema export. */
    readonly modules: number;
    /** The number of directly exported Schema values found in those modules. */
    readonly schemas: number;
}
declare const BuildError_base: new <A extends Record<string, any> = {}>(args: import("../../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../../Cause.ts").YieldableError & {
    readonly _tag: "BuildError";
} & Readonly<A>;
/**
 * An error raised while loading modules or generating an AOT module.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class BuildError extends BuildError_base<{
    readonly _tag: "BuildError";
    readonly cause: unknown;
    readonly kind: "Generate" | "InvalidModule" | "LoadModule" | "ResolveModule";
    readonly message: string;
    readonly module?: string | undefined;
}> {
}
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
export declare const build: (options: BuildOptions) => Effect.Effect<BuildResult, BuildError | PlatformError.PlatformError, FileSystem.FileSystem | Path.Path>;
export {};
//# sourceMappingURL=Build.d.ts.map