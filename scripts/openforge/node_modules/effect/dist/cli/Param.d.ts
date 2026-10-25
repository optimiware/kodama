/**
 * Defines the shared parameter model for CLI arguments and flags.
 *
 * A `Param<Kind, A>` describes how to consume parsed command-line input and
 * return a typed value. The `Kind` decides whether the parameter reads
 * positional arguments or named flags. `Argument` and `Flag` build on this
 * module to share parsing structure, primitive constructors, help metadata,
 * aliases, defaults, prompts, configuration fallbacks, validation, schema
 * decoding, fallback parameters, and traversal helpers.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Config from "../Config.ts";
import * as Effect from "../Effect.ts";
import { type LazyArg } from "../Function.ts";
import * as Option from "../Option.ts";
import { type Pipeable } from "../Pipeable.ts";
import * as Predicate from "../Predicate.ts";
import type * as Redacted_ from "../Redacted.ts";
import * as Result from "../Result.ts";
import * as Schema from "../Schema.ts";
import type { Covariant } from "../Types.ts";
import * as CliError from "./CliError.ts";
import type { Environment } from "./Command.ts";
import * as Primitive from "./Primitive.ts";
import * as Prompt from "./Prompt.ts";
declare const TypeId = "~effect/cli/Param";
/**
 * Polymorphic CLI parameter shared by `Argument` and `Flag`.
 *
 * **Details**
 *
 * A parameter knows whether it consumes positional arguments or flags and
 * parses a `ParsedArgs` value into its typed result.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Param<Kind extends ParamKind, out A> extends Param.Variance<A> {
    readonly _tag: "Single" | "Map" | "Transform" | "Optional" | "Variadic";
    readonly kind: Kind;
    readonly parse: Parse<A>;
}
/**
 * Discriminator for whether a `Param` parses positional arguments or
 * command-line flags.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ParamKind = "argument" | "flag";
/**
 * Defines the kind discriminator for positional argument parameters.
 *
 * **When to use**
 *
 * Use to build low-level `Param` constructors or type positions for positional
 * argument parameters.
 *
 * @see {@link flagKind} for the named flag parameter discriminator
 * @see {@link ParamKind} for the full parameter kind union
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const argumentKind: "argument";
/**
 * Defines the kind discriminator for flag parameters.
 *
 * **When to use**
 *
 * Use to build low-level `Param` constructors or type positions for named flag
 * parameters.
 *
 * @see {@link argumentKind} for the positional argument parameter discriminator
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const flagKind: "flag";
/**
 * Represents any parameter.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Any = Param<ParamKind, unknown>;
/**
 * Represents any positional argument parameter.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type AnyArgument = Param<typeof argumentKind, unknown>;
/**
 * Represents any flag parameter.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type AnyFlag = Param<typeof flagKind, unknown>;
/**
 * Function type used by parameters to parse currently available flags and
 * positional arguments.
 *
 * **Details**
 *
 * It returns the remaining positional arguments together with the parsed value,
 * or fails with a `CliError` while requiring the CLI parsing environment.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Parse<A> = (args: ParsedArgs) => Effect.Effect<readonly [leftover: ReadonlyArray<string>, value: A], CliError.CliError, Environment>;
/**
 * Namespace containing type-level utilities attached to the `Param` interface.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Param {
    /**
     * Variance and pipeability marker carried by every `Param` value.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    interface Variance<out A> extends Pipeable {
        readonly [TypeId]: {
            readonly _A: Covariant<A>;
        };
    }
}
/**
 * Map of flag names to their provided string values.
 * Multiple occurrences of a flag produce multiple values.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Flags = Record<string, ReadonlyArray<string>>;
/**
 * Input context passed to `Param.parse` implementations.
 * - `flags`: already-collected flag values by canonical flag name
 * - `arguments`: remaining positional arguments to be consumed
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface ParsedArgs {
    readonly flags: Flags;
    readonly arguments: ReadonlyArray<string>;
}
/**
 * Represents a fallback prompt that can either be provided directly or
 * computed effectfully when the parameter is missing.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type FallbackPrompt<A> = Prompt.Prompt<A> | Effect.Effect<Prompt.Prompt<A>, CliError.CliError, Environment>;
/**
 * Leaf parameter that reads one named argument or flag with a primitive parser.
 *
 * **Details**
 *
 * Single parameters carry the user-facing name, aliases, description, primitive
 * type, and optional metavar/type name used in help output.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Single<Kind extends ParamKind, out A> extends Param<Kind, A> {
    readonly _tag: "Single";
    readonly kind: Kind;
    readonly name: string;
    readonly description: Option.Option<string>;
    readonly aliases: ReadonlyArray<string>;
    readonly primitiveType: Primitive.Primitive<A>;
    readonly typeName?: string | undefined;
    readonly hidden: boolean;
}
/**
 * Parameter node that maps the successfully parsed value of another parameter
 * with a pure function.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Map<Kind extends ParamKind, in out A, out B> extends Param<Kind, B> {
    readonly _tag: "Map";
    readonly kind: Kind;
    readonly param: Param<Kind, A>;
    readonly f: (value: A) => B;
}
/**
 * Parameter node that rewrites another parameter's parser, allowing effectful
 * validation, fallback behavior, or error translation while preserving the same
 * parameter kind.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Transform<Kind extends ParamKind, in out A, out B> extends Param<Kind, B> {
    readonly _tag: "Transform";
    readonly kind: Kind;
    readonly param: Param<Kind, A>;
    readonly alternatives: ReadonlyArray<LazyArg<Param<Kind, unknown>>>;
    readonly f: (parse: Parse<A>, alternatives: ReadonlyArray<LazyArg<Parse<unknown>>>) => Parse<B>;
}
/**
 * Parameter node that turns a missing argument or flag into `Option.none()` and
 * a present parsed value into `Option.some(value)`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Optional<Kind extends ParamKind, A> extends Param<Kind, Option.Option<A>> {
    readonly _tag: "Optional";
    readonly kind: Kind;
    readonly param: Param<Kind, A>;
}
/**
 * Parameter node that parses another parameter zero or more times and returns
 * all parsed values as an array, respecting optional minimum and maximum
 * occurrence bounds.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Variadic<Kind extends ParamKind, A> extends Param<Kind, ReadonlyArray<A>> {
    readonly _tag: "Variadic";
    readonly kind: Kind;
    readonly param: Param<Kind, A>;
    readonly min: Option.Option<number>;
    readonly max: Option.Option<number>;
}
/**
 * Type guard to check if a value is a Param.
 *
 * **Example** (Checking for params)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const maybeParam = Param.String(Param.flagKind, "name")
 *
 * Param.isParam(maybeParam) // => true
 * ```
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isParam: (u: unknown) => u is Param<any, ParamKind>;
/**
 * Type guard to check if a param is a Single param (not composed).
 *
 * **Example** (Checking for single params)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const nameParam = Param.String(Param.flagKind, "name")
 * const optionalParam = Param.optional(nameParam)
 *
 * Param.isSingle(nameParam) // => true
 * Param.isSingle(optionalParam) // => false
 * ```
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isSingle: <const Kind extends ParamKind, A>(param: Param<Kind, A>) => param is Single<Kind, A>;
/**
 * Constructs a leaf `Single` parameter from its kind, name, primitive parser,
 * and optional help metadata.
 *
 * **Details**
 *
 * The returned parser reads either one positional argument or the named flag,
 * depending on `kind`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeSingle: <const Kind extends ParamKind, A>(params: {
    readonly kind: Kind;
    readonly name: string;
    readonly primitiveType: Primitive.Primitive<A>;
    readonly typeName?: string | undefined;
    readonly description?: Option.Option<string> | undefined;
    readonly aliases?: ReadonlyArray<string> | undefined;
    readonly hidden?: boolean | undefined;
}) => Single<Kind, A>;
/**
 * Creates a string parameter.
 *
 * **Example** (Creating string parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Create a string flag
 * const nameFlag = Param.String(Param.flagKind, "name")
 *
 * // Create a string argument
 * const fileArg = Param.String(Param.argumentKind, "file")
 *
 * // Usage in CLI: --name "John Doe" or as positional argument
 * const kinds = [nameFlag.kind, fileArg.kind] // => ["flag", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const String: <const Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, string>;
/**
 * Creates a boolean parameter.
 *
 * **Example** (Creating boolean parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Create a boolean flag
 * const verboseFlag = Param.Boolean(Param.flagKind, "verbose")
 *
 * // Create a boolean argument
 * const enableArg = Param.Boolean(Param.argumentKind, "enable")
 *
 * // Usage in CLI: --verbose (true) or --no-verbose (false).
 * // The flag is required unless made optional or given a fallback.
 * // Boolean positional arguments accept true/false.
 * const kinds = [verboseFlag.kind, enableArg.kind] // => ["flag", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Boolean: <const Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, boolean>;
/**
 * Creates an integer parameter.
 *
 * **Example** (Creating integer parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Create an integer flag
 * const portFlag = Param.Int(Param.flagKind, "port")
 *
 * // Create an integer argument
 * const countArg = Param.Int(Param.argumentKind, "count")
 *
 * // Usage in CLI: --port 8080 or as positional argument: 42
 * const kinds = [portFlag.kind, countArg.kind] // => ["flag", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Int: <const Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, number>;
/**
 * Creates a finite number parameter.
 *
 * **Example** (Creating finite number parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const rateFlag = Param.Finite(Param.flagKind, "rate")
 *
 * const thresholdArg = Param.Finite(Param.argumentKind, "threshold")
 *
 * const kinds = [rateFlag.kind, thresholdArg.kind] // => ["flag", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Finite: <const Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, number>;
/**
 * Creates a date parameter that parses ISO date strings.
 *
 * **Example** (Creating date parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Create a date flag
 * const startFlag = Param.Date(Param.flagKind, "start-date")
 *
 * // Create a date argument
 * const dueDateArg = Param.Date(Param.argumentKind, "due-date")
 *
 * // Usage in CLI: --start-date "2023-12-25" or as positional: "2023-01-01"
 * // Parses to JavaScript Date object
 * const kinds = [startFlag.kind, dueDateArg.kind] // => ["flag", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Date: <const Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, globalThis.Date>;
/**
 * Constructs command-line params that represent a choice between several
 * inputs. The input will be mapped to it's associated value during parsing.
 *
 * **Example** (Creating valued choices)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * type Animal = Dog | Cat
 *
 * interface Dog {
 *   readonly _tag: "Dog"
 * }
 *
 * interface Cat {
 *   readonly _tag: "Cat"
 * }
 *
 * const animal = Param.ChoiceWithValue(Param.flagKind, "animal", [
 *   ["dog", { _tag: "Dog" }],
 *   ["cat", { _tag: "Cat" }]
 * ])
 * animal.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const ChoiceWithValue: <const Kind extends ParamKind, const Choices extends ReadonlyArray<readonly [string, any]>>(kind: Kind, name: string, choices: Choices) => Param<Kind, Choices[number][1]>;
/**
 * Constructs command-line params that represent a choice between several
 * string inputs.
 *
 * **Example** (Creating string choices)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const logLevel = Param.Literals(Param.flagKind, "log-level", [
 *   "debug",
 *   "info",
 *   "warn",
 *   "error"
 * ])
 * logLevel.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Literals: <const Kind extends ParamKind, const Choices extends ReadonlyArray<string>>(kind: Kind, name: string, choices: Choices) => Param<Kind, Choices[number]>;
/**
 * Creates a path parameter that accepts file or directory paths.
 *
 * **Example** (Creating path parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Basic path parameter
 * const outputPath = Param.Path(Param.flagKind, "output")
 *
 * // Path that must exist
 * const inputPath = Param.Path(Param.flagKind, "input", { mustExist: true })
 *
 * // File-only path
 * const configFile = Param.Path(Param.flagKind, "config", {
 *   pathType: "file",
 *   mustExist: true,
 *   typeName: "config-file"
 * })
 * const kinds = [outputPath.kind, inputPath.kind, configFile.kind] // => ["flag", "flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Path: <Kind extends ParamKind>(kind: Kind, name: string, options?: {
    readonly pathType?: Primitive.PathType | undefined;
    readonly mustExist?: boolean | undefined;
    readonly typeName?: string | undefined;
}) => Param<Kind, string>;
/**
 * Creates a directory path parameter.
 *
 * **Details**
 *
 * This is a convenience function that creates a path parameter with the
 * `pathType` set to `"directory"` and a default type name of `"directory"`.
 *
 * **Example** (Creating directory parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Basic directory parameter
 * const outputDir = Param.Directory(Param.flagKind, "output-dir")
 *
 * // Directory that must exist
 * const sourceDir = Param.Directory(Param.flagKind, "source", { mustExist: true })
 *
 * // Usage: --output-dir /path/to/dir --source /existing/dir
 * const kinds = [outputDir.kind, sourceDir.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Directory: <Kind extends ParamKind>(kind: Kind, name: string, options?: {
    readonly mustExist?: boolean | undefined;
}) => Param<Kind, string>;
/**
 * Creates a file path parameter.
 *
 * **Details**
 *
 * This is a convenience function that creates a path parameter with a
 * `pathType` set to `"file"` and a default type name of `"file"`.
 *
 * **Example** (Creating file parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Basic file parameter
 * const outputFile = Param.File(Param.flagKind, "output")
 *
 * // File that must exist
 * const inputFile = Param.File(Param.flagKind, "input", { mustExist: true })
 *
 * // Usage: --output result.txt --input existing-file.txt
 * const kinds = [outputFile.kind, inputFile.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const File: <Kind extends ParamKind>(kind: Kind, name: string, options?: {
    readonly mustExist?: boolean | undefined;
}) => Param<Kind, string>;
/**
 * Creates a redacted parameter for sensitive data like passwords.
 * The value is masked in help output and logging.
 *
 * **Example** (Creating redacted parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Create a password parameter
 * const password = Param.Redacted(Param.flagKind, "password")
 *
 * // Create an API key argument
 * const apiKey = Param.Redacted(Param.argumentKind, "api-key")
 *
 * // Usage: --password (value will be hidden in help/logs)
 * const kinds = [password.kind, apiKey.kind] // => ["flag", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Redacted: <Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, Redacted_.Redacted<string>>;
/**
 * Creates a parameter that reads and returns file content as a string.
 *
 * **Example** (Reading file text)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Read a config file as string
 * const configContent = Param.FileText(Param.flagKind, "config")
 *
 * // Read a template file as argument
 * const templateContent = Param.FileText(Param.argumentKind, "template")
 *
 * // Usage: --config config.txt (reads file content into string)
 * const kinds = [configContent.kind, templateContent.kind] // => ["flag", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const FileText: <Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, string>;
/**
 * Creates a param that reads and parses the content of the specified file.
 *
 * **Details**
 *
 * The parser that is utilized will depend on the specified `format`, or the
 * extension of the file passed on the command-line if no `format` is specified.
 *
 * **Example** (Parsing file contents)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Will use the extension of the file passed on the command line to determine
 * // the parser to use
 * const config = Param.FileParse(Param.flagKind, "config")
 *
 * // Will use the JSON parser
 * const jsonConfig = Param.FileParse(Param.flagKind, "json-config", {
 *   format: "json"
 * })
 * const kinds = [config.kind, jsonConfig.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const FileParse: <Kind extends ParamKind>(kind: Kind, name: string, options?: Primitive.FileParseOptions | undefined) => Param<Kind, unknown>;
/**
 * Creates a parameter that reads and validates file content using a schema.
 *
 * **Example** (Validating file contents)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { Param } from "effect/cli"
 * // Parse JSON config file
 * const configSchema = Schema.Struct({
 *   port: Schema.Number,
 *   host: Schema.String
 * })
 *
 * const config = Param.FileSchema(Param.flagKind, "config", configSchema, {
 *   format: "json"
 * })
 *
 * // Parse YAML file
 * const yamlConfig = Param.FileSchema(Param.flagKind, "config", configSchema, {
 *   format: "yaml"
 * })
 *
 * // Usage: --config config.json (reads and validates file content)
 * const kinds = [config.kind, yamlConfig.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const FileSchema: <Kind extends ParamKind, A>(kind: Kind, name: string, schema: Schema.ConstraintDecoder<A, Environment>, options?: Primitive.FileSchemaOptions | undefined) => Param<Kind, A>;
/**
 * Creates a param that parses key=value pairs.
 *
 * **When to use**
 *
 * Use when you need command-line options or arguments that collect `key=value`
 * configuration entries.
 *
 * **Details**
 *
 * Requires at least one key=value pair. The parsed pairs are merged into a
 * single record object.
 *
 * **Example** (Parsing key-value pairs)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const env = Param.KeyValuePair(Param.flagKind, "env")
 * // --env FOO=bar --env BAZ=qux will parse to { FOO: "bar", BAZ: "qux" }
 *
 * const props = Param.KeyValuePair(Param.flagKind, "property")
 * // --property name=value --property debug=true
 * const kinds = [env.kind, props.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const KeyValuePair: <Kind extends ParamKind>(kind: Kind, name: string) => Param<Kind, Record<string, string>>;
/**
 * A parameter that always fails to parse.
 *
 * **Example** (Creating sentinel parameters)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const disabledDebugParam = Param.Never(Param.flagKind)
 *
 * const makeDebugParam = (enableDebug: boolean) =>
 *   enableDebug ? Param.String(Param.flagKind, "debug") : disabledDebugParam
 *
 * makeDebugParam(true) === disabledDebugParam // => false
 * makeDebugParam(false) === disabledDebugParam // => true
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Never: <Kind extends ParamKind>(kind: Kind) => Param<Kind, never>;
/**
 * Adds an alias to an option.
 *
 * **When to use**
 *
 * Use when you need a CLI parameter to accept an alternate name, such as "-f"
 * for "--force".
 *
 * **Details**
 *
 * This works on any param structure by recursively finding the underlying
 * `Single` node and applying the alias there.
 *
 * **Example** (Adding parameter aliases)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const force = Param.Boolean(Param.flagKind, "force").pipe(
 *   Param.withAlias("-f"),
 *   Param.withAlias("-F")
 * )
 *
 * // Also works on composed params:
 * const count = Param.Int(Param.flagKind, "count").pipe(
 *   Param.optional,
 *   Param.withAlias("-c") // finds the underlying Single and adds alias
 * )
 * const kinds = [force.kind, count.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const withAlias: {
    /**
     * Adds an alias to an option.
     *
     * **When to use**
     *
     * Use when you need a CLI parameter to accept an alternate name, such as "-f"
     * for "--force".
     *
     * **Details**
     *
     * This works on any param structure by recursively finding the underlying
     * `Single` node and applying the alias there.
     *
     * **Example** (Adding parameter aliases)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const force = Param.Boolean(Param.flagKind, "force").pipe(
     *   Param.withAlias("-f"),
     *   Param.withAlias("-F")
     * )
     *
     * // Also works on composed params:
     * const count = Param.Int(Param.flagKind, "count").pipe(
     *   Param.optional,
     *   Param.withAlias("-c") // finds the underlying Single and adds alias
     * )
     * const kinds = [force.kind, count.kind] // => ["flag", "flag"]
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(alias: string): (self: Param<Kind, A>) => Param<Kind, A>;
    /**
     * Adds an alias to an option.
     *
     * **When to use**
     *
     * Use when you need a CLI parameter to accept an alternate name, such as "-f"
     * for "--force".
     *
     * **Details**
     *
     * This works on any param structure by recursively finding the underlying
     * `Single` node and applying the alias there.
     *
     * **Example** (Adding parameter aliases)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const force = Param.Boolean(Param.flagKind, "force").pipe(
     *   Param.withAlias("-f"),
     *   Param.withAlias("-F")
     * )
     *
     * // Also works on composed params:
     * const count = Param.Int(Param.flagKind, "count").pipe(
     *   Param.optional,
     *   Param.withAlias("-c") // finds the underlying Single and adds alias
     * )
     * const kinds = [force.kind, count.kind] // => ["flag", "flag"]
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(self: Param<Kind, A>, alias: string): Param<Kind, A>;
};
/**
 * Adds a description to an option for help text.
 *
 * **Details**
 *
 * Descriptions provide users with information about what the option does
 * when they view help documentation.
 *
 * **Example** (Adding help descriptions)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const verbose = Param.Boolean(Param.flagKind, "verbose").pipe(
 *   Param.withAlias("-v"),
 *   Param.withDescription("Enable verbose output")
 * )
 * verbose.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const withDescription: {
    /**
     * Adds a description to an option for help text.
     *
     * **Details**
     *
     * Descriptions provide users with information about what the option does
     * when they view help documentation.
     *
     * **Example** (Adding help descriptions)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const verbose = Param.Boolean(Param.flagKind, "verbose").pipe(
     *   Param.withAlias("-v"),
     *   Param.withDescription("Enable verbose output")
     * )
     * verbose.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(description: string): (self: Param<Kind, A>) => Param<Kind, A>;
    /**
     * Adds a description to an option for help text.
     *
     * **Details**
     *
     * Descriptions provide users with information about what the option does
     * when they view help documentation.
     *
     * **Example** (Adding help descriptions)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const verbose = Param.Boolean(Param.flagKind, "verbose").pipe(
     *   Param.withAlias("-v"),
     *   Param.withDescription("Enable verbose output")
     * )
     * verbose.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(self: Param<Kind, A>, description: string): Param<Kind, A>;
};
/**
 * Hides a parameter from generated help output and completions while keeping
 * it parseable on the command line.
 *
 * **When to use**
 *
 * Use when experimental, internal, or deprecated flags should be accepted but
 * not advertised.
 *
 * **Example** (Hiding a flag from help)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const experimental = Param.Boolean(Param.flagKind, "experimental-foo").pipe(
 *   Param.withHidden
 * )
 * experimental.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category metadata
 * @since 4.0.0
 */
export declare const withHidden: <Kind extends ParamKind, A>(self: Param<Kind, A>) => Param<Kind, A>;
/**
 * Transforms the parsed value of an option using a mapping function.
 *
 * **Example** (Mapping parsed values)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const port = Param.Int(Param.flagKind, "port").pipe(
 *   Param.map((n) => ({ port: n, url: `http://localhost:${n}` }))
 * )
 * port.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const map: {
    /**
     * Transforms the parsed value of an option using a mapping function.
     *
     * **Example** (Mapping parsed values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const port = Param.Int(Param.flagKind, "port").pipe(
     *   Param.map((n) => ({ port: n, url: `http://localhost:${n}` }))
     * )
     * port.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A, B>(f: (a: A) => B): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, B>;
    /**
     * Transforms the parsed value of an option using a mapping function.
     *
     * **Example** (Mapping parsed values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const port = Param.Int(Param.flagKind, "port").pipe(
     *   Param.map((n) => ({ port: n, url: `http://localhost:${n}` }))
     * )
     * port.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, f: (a: A) => B): Param<Kind, B>;
};
/**
 * Transforms the parsed value of an option using an effectful mapping function.
 *
 * **Example** (Mapping parsed values effectfully)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { CliError, Param } from "effect/cli"
 * import { ChildProcessSpawner } from "effect/process"
 *
 * const CliTestLayer = Layer.mergeAll(
 *   FileSystem.layerNoop({}),
 *   Path.layer,
 *   Stdio.layerTest({}),
 *   Layer.succeed(Terminal.Terminal, Terminal.make({
 *     columns: Effect.succeed(80),
 *     rows: Effect.succeed(24),
 *     readInput: Effect.die("unused"),
 *     readLine: Effect.die("unused"),
 *     display: () => Effect.void
 *   })),
 *   Layer.succeed(
 *     ChildProcessSpawner.ChildProcessSpawner,
 *     ChildProcessSpawner.make(() => Effect.die("unused"))
 *   )
 * )
 *
 * const validatedEmail = Param.String(Param.flagKind, "email").pipe(
 *   Param.mapEffect((email) =>
 *     email.includes("@")
 *       ? Effect.succeed(email)
 *       : Effect.fail(
 *         new CliError.InvalidValue({
 *           option: "email",
 *           value: email,
 *           expected: "valid email format",
 *           kind: "flag"
 *         })
 *       )
 *   )
 * )
 *
 * const [, value] = await Effect.runPromise(
 *   validatedEmail.parse({
 *     arguments: [],
 *     flags: { email: ["alice@example.com"] }
 *   }).pipe(Effect.provide(CliTestLayer))
 * )
 * value // => "alice@example.com"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const mapEffect: {
    /**
     * Transforms the parsed value of an option using an effectful mapping function.
     *
     * **Example** (Mapping parsed values effectfully)
     *
     * ```ts import.meta.vitest
     * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
     * import { CliError, Param } from "effect/cli"
     * import { ChildProcessSpawner } from "effect/process"
     *
     * const CliTestLayer = Layer.mergeAll(
     *   FileSystem.layerNoop({}),
     *   Path.layer,
     *   Stdio.layerTest({}),
     *   Layer.succeed(Terminal.Terminal, Terminal.make({
     *     columns: Effect.succeed(80),
     *     rows: Effect.succeed(24),
     *     readInput: Effect.die("unused"),
     *     readLine: Effect.die("unused"),
     *     display: () => Effect.void
     *   })),
     *   Layer.succeed(
     *     ChildProcessSpawner.ChildProcessSpawner,
     *     ChildProcessSpawner.make(() => Effect.die("unused"))
     *   )
     * )
     *
     * const validatedEmail = Param.String(Param.flagKind, "email").pipe(
     *   Param.mapEffect((email) =>
     *     email.includes("@")
     *       ? Effect.succeed(email)
     *       : Effect.fail(
     *         new CliError.InvalidValue({
     *           option: "email",
     *           value: email,
     *           expected: "valid email format",
     *           kind: "flag"
     *         })
     *       )
     *   )
     * )
     *
     * const [, value] = await Effect.runPromise(
     *   validatedEmail.parse({
     *     arguments: [],
     *     flags: { email: ["alice@example.com"] }
     *   }).pipe(Effect.provide(CliTestLayer))
     * )
     * value // => "alice@example.com"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A, B>(f: (a: A) => Effect.Effect<B, CliError.CliError, Environment>): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, B>;
    /**
     * Transforms the parsed value of an option using an effectful mapping function.
     *
     * **Example** (Mapping parsed values effectfully)
     *
     * ```ts import.meta.vitest
     * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
     * import { CliError, Param } from "effect/cli"
     * import { ChildProcessSpawner } from "effect/process"
     *
     * const CliTestLayer = Layer.mergeAll(
     *   FileSystem.layerNoop({}),
     *   Path.layer,
     *   Stdio.layerTest({}),
     *   Layer.succeed(Terminal.Terminal, Terminal.make({
     *     columns: Effect.succeed(80),
     *     rows: Effect.succeed(24),
     *     readInput: Effect.die("unused"),
     *     readLine: Effect.die("unused"),
     *     display: () => Effect.void
     *   })),
     *   Layer.succeed(
     *     ChildProcessSpawner.ChildProcessSpawner,
     *     ChildProcessSpawner.make(() => Effect.die("unused"))
     *   )
     * )
     *
     * const validatedEmail = Param.String(Param.flagKind, "email").pipe(
     *   Param.mapEffect((email) =>
     *     email.includes("@")
     *       ? Effect.succeed(email)
     *       : Effect.fail(
     *         new CliError.InvalidValue({
     *           option: "email",
     *           value: email,
     *           expected: "valid email format",
     *           kind: "flag"
     *         })
     *       )
     *   )
     * )
     *
     * const [, value] = await Effect.runPromise(
     *   validatedEmail.parse({
     *     arguments: [],
     *     flags: { email: ["alice@example.com"] }
     *   }).pipe(Effect.provide(CliTestLayer))
     * )
     * value // => "alice@example.com"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, f: (a: A) => Effect.Effect<B, CliError.CliError, Environment>): Param<Kind, B>;
};
/**
 * Transforms the parsed value of an option using a function that may throw,
 * converting any thrown errors into failure messages.
 *
 * **Example** (Mapping thrown errors)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Param } from "effect/cli"
 * import { ChildProcessSpawner } from "effect/process"
 *
 * const CliTestLayer = Layer.mergeAll(
 *   FileSystem.layerNoop({}),
 *   Path.layer,
 *   Stdio.layerTest({}),
 *   Layer.succeed(Terminal.Terminal, Terminal.make({
 *     columns: Effect.succeed(80),
 *     rows: Effect.succeed(24),
 *     readInput: Effect.die("unused"),
 *     readLine: Effect.die("unused"),
 *     display: () => Effect.void
 *   })),
 *   Layer.succeed(
 *     ChildProcessSpawner.ChildProcessSpawner,
 *     ChildProcessSpawner.make(() => Effect.die("unused"))
 *   )
 * )
 *
 * const parsedJson = Param.String(Param.flagKind, "config").pipe(
 *   Param.mapTryCatch(
 *     (str) => JSON.parse(str),
 *     (error) =>
 *       `Invalid JSON: ${error instanceof Error ? error.message : String(error)}`
 *   )
 * )
 *
 * const [, value] = await Effect.runPromise(
 *   parsedJson.parse({
 *     arguments: [],
 *     flags: { config: ['{"enabled":true}'] }
 *   }).pipe(Effect.provide(CliTestLayer))
 * )
 * value // => { enabled: true }
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const mapTryCatch: {
    /**
     * Transforms the parsed value of an option using a function that may throw,
     * converting any thrown errors into failure messages.
     *
     * **Example** (Mapping thrown errors)
     *
     * ```ts import.meta.vitest
     * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
     * import { Param } from "effect/cli"
     * import { ChildProcessSpawner } from "effect/process"
     *
     * const CliTestLayer = Layer.mergeAll(
     *   FileSystem.layerNoop({}),
     *   Path.layer,
     *   Stdio.layerTest({}),
     *   Layer.succeed(Terminal.Terminal, Terminal.make({
     *     columns: Effect.succeed(80),
     *     rows: Effect.succeed(24),
     *     readInput: Effect.die("unused"),
     *     readLine: Effect.die("unused"),
     *     display: () => Effect.void
     *   })),
     *   Layer.succeed(
     *     ChildProcessSpawner.ChildProcessSpawner,
     *     ChildProcessSpawner.make(() => Effect.die("unused"))
     *   )
     * )
     *
     * const parsedJson = Param.String(Param.flagKind, "config").pipe(
     *   Param.mapTryCatch(
     *     (str) => JSON.parse(str),
     *     (error) =>
     *       `Invalid JSON: ${error instanceof Error ? error.message : String(error)}`
     *   )
     * )
     *
     * const [, value] = await Effect.runPromise(
     *   parsedJson.parse({
     *     arguments: [],
     *     flags: { config: ['{"enabled":true}'] }
     *   }).pipe(Effect.provide(CliTestLayer))
     * )
     * value // => { enabled: true }
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A, B>(f: (a: A) => B, onError: (error: unknown) => string): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, B>;
    /**
     * Transforms the parsed value of an option using a function that may throw,
     * converting any thrown errors into failure messages.
     *
     * **Example** (Mapping thrown errors)
     *
     * ```ts import.meta.vitest
     * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
     * import { Param } from "effect/cli"
     * import { ChildProcessSpawner } from "effect/process"
     *
     * const CliTestLayer = Layer.mergeAll(
     *   FileSystem.layerNoop({}),
     *   Path.layer,
     *   Stdio.layerTest({}),
     *   Layer.succeed(Terminal.Terminal, Terminal.make({
     *     columns: Effect.succeed(80),
     *     rows: Effect.succeed(24),
     *     readInput: Effect.die("unused"),
     *     readLine: Effect.die("unused"),
     *     display: () => Effect.void
     *   })),
     *   Layer.succeed(
     *     ChildProcessSpawner.ChildProcessSpawner,
     *     ChildProcessSpawner.make(() => Effect.die("unused"))
     *   )
     * )
     *
     * const parsedJson = Param.String(Param.flagKind, "config").pipe(
     *   Param.mapTryCatch(
     *     (str) => JSON.parse(str),
     *     (error) =>
     *       `Invalid JSON: ${error instanceof Error ? error.message : String(error)}`
     *   )
     * )
     *
     * const [, value] = await Effect.runPromise(
     *   parsedJson.parse({
     *     arguments: [],
     *     flags: { config: ['{"enabled":true}'] }
     *   }).pipe(Effect.provide(CliTestLayer))
     * )
     * value // => { enabled: true }
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, f: (a: A) => B, onError: (error: unknown) => string): Param<Kind, B>;
};
/**
 * Makes a flag or positional argument optional.
 *
 * **Details**
 *
 * When the parameter is absent, parsing succeeds with `Option.none()` instead
 * of failing with a missing option or missing argument error. When present, the
 * parsed value is wrapped in `Option.some()`.
 *
 * **Example** (Making parameters optional)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Create an optional port option
 * // - When not provided: returns Option.none()
 * // - When provided: returns Option.some(parsedValue)
 * const port = Param.optional(Param.Int(Param.flagKind, "port"))
 * port.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const optional: <Kind extends ParamKind, A>(param: Param<Kind, A>) => Param<Kind, Option.Option<A>>;
/**
 * Makes a flag or positional argument optional by supplying a fallback value.
 *
 * **Details**
 *
 * The fallback may be a pure value or an effect. It is used only when the
 * parameter is absent; provided values are parsed normally.
 *
 * **Example** (Providing default values)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Using the pipe operator to make an option optional
 * const port = Param.Int(Param.flagKind, "port").pipe(
 *   Param.withDefault(8080)
 * )
 *
 * // Can also be used with other combinators
 * const verbose = Param.Boolean(Param.flagKind, "verbose").pipe(
 *   Param.withAlias("-v"),
 *   Param.withDescription("Enable verbose output"),
 *   Param.withDefault(false)
 * )
 * const kinds = [port.kind, verbose.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const withDefault: {
    /**
     * Makes a flag or positional argument optional by supplying a fallback value.
     *
     * **Details**
     *
     * The fallback may be a pure value or an effect. It is used only when the
     * parameter is absent; provided values are parsed normally.
     *
     * **Example** (Providing default values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Using the pipe operator to make an option optional
     * const port = Param.Int(Param.flagKind, "port").pipe(
     *   Param.withDefault(8080)
     * )
     *
     * // Can also be used with other combinators
     * const verbose = Param.Boolean(Param.flagKind, "verbose").pipe(
     *   Param.withAlias("-v"),
     *   Param.withDescription("Enable verbose output"),
     *   Param.withDefault(false)
     * )
     * const kinds = [port.kind, verbose.kind] // => ["flag", "flag"]
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <const B>(defaultValue: B | Effect.Effect<B, CliError.CliError, Environment>): <Kind extends ParamKind, A>(self: Param<Kind, A>) => Param<Kind, A | B>;
    /**
     * Makes a flag or positional argument optional by supplying a fallback value.
     *
     * **Details**
     *
     * The fallback may be a pure value or an effect. It is used only when the
     * parameter is absent; provided values are parsed normally.
     *
     * **Example** (Providing default values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Using the pipe operator to make an option optional
     * const port = Param.Int(Param.flagKind, "port").pipe(
     *   Param.withDefault(8080)
     * )
     *
     * // Can also be used with other combinators
     * const verbose = Param.Boolean(Param.flagKind, "verbose").pipe(
     *   Param.withAlias("-v"),
     *   Param.withDescription("Enable verbose output"),
     *   Param.withDefault(false)
     * )
     * const kinds = [port.kind, verbose.kind] // => ["flag", "flag"]
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, const B>(self: Param<Kind, A>, defaultValue: B | Effect.Effect<B, CliError.CliError, Environment>): Param<Kind, A | B>;
};
/**
 * Adds a fallback config that is loaded when a required parameter is missing.
 *
 * **When to use**
 *
 * Use when you need config to provide a fallback source for required flags or
 * arguments that are absent from CLI input.
 *
 * **Details**
 *
 * Provided CLI values win. Config is loaded only after a missing option or
 * missing argument error.
 *
 * **Gotchas**
 *
 * Missing config preserves the original missing-parameter error. Config parse
 * failure becomes `CliError.InvalidValue`.
 *
 * @see {@link withDefault} for a pure default value
 * @see {@link withFallbackPrompt} for prompting interactively when input is missing
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const withFallbackConfig: {
    /**
     * Adds a fallback config that is loaded when a required parameter is missing.
     *
     * **When to use**
     *
     * Use when you need config to provide a fallback source for required flags or
     * arguments that are absent from CLI input.
     *
     * **Details**
     *
     * Provided CLI values win. Config is loaded only after a missing option or
     * missing argument error.
     *
     * **Gotchas**
     *
     * Missing config preserves the original missing-parameter error. Config parse
     * failure becomes `CliError.InvalidValue`.
     *
     * @see {@link withDefault} for a pure default value
     * @see {@link withFallbackPrompt} for prompting interactively when input is missing
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <B>(config: Config.Config<B>): <Kind extends ParamKind, A>(self: Param<Kind, A>) => Param<Kind, A | B>;
    /**
     * Adds a fallback config that is loaded when a required parameter is missing.
     *
     * **When to use**
     *
     * Use when you need config to provide a fallback source for required flags or
     * arguments that are absent from CLI input.
     *
     * **Details**
     *
     * Provided CLI values win. Config is loaded only after a missing option or
     * missing argument error.
     *
     * **Gotchas**
     *
     * Missing config preserves the original missing-parameter error. Config parse
     * failure becomes `CliError.InvalidValue`.
     *
     * @see {@link withDefault} for a pure default value
     * @see {@link withFallbackPrompt} for prompting interactively when input is missing
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, config: Config.Config<B>): Param<Kind, A | B>;
};
/**
 * Adds a fallback prompt that is shown when a required parameter is missing.
 *
 * **When to use**
 *
 * Use when a CLI should ask interactively for a missing required flag or
 * argument.
 *
 * **Details**
 *
 * `FallbackPrompt` accepts either a `Prompt` or an effect that builds one.
 * Effectful prompt creation is lazy and runs only when the fallback is needed.
 *
 * **Gotchas**
 *
 * This only handles missing options and missing arguments. Invalid values do not
 * prompt, and prompt cancellation re-fails with the original missing error.
 *
 * @see {@link FallbackPrompt} for accepted fallback prompt forms
 * @see {@link withFallbackConfig} for loading a fallback from config
 * @see {@link withDefault} for a pure default value
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const withFallbackPrompt: {
    /**
     * Adds a fallback prompt that is shown when a required parameter is missing.
     *
     * **When to use**
     *
     * Use when a CLI should ask interactively for a missing required flag or
     * argument.
     *
     * **Details**
     *
     * `FallbackPrompt` accepts either a `Prompt` or an effect that builds one.
     * Effectful prompt creation is lazy and runs only when the fallback is needed.
     *
     * **Gotchas**
     *
     * This only handles missing options and missing arguments. Invalid values do not
     * prompt, and prompt cancellation re-fails with the original missing error.
     *
     * @see {@link FallbackPrompt} for accepted fallback prompt forms
     * @see {@link withFallbackConfig} for loading a fallback from config
     * @see {@link withDefault} for a pure default value
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <B>(prompt: FallbackPrompt<B>): <Kind extends ParamKind, A>(self: Param<Kind, A>) => Param<Kind, A | B>;
    /**
     * Adds a fallback prompt that is shown when a required parameter is missing.
     *
     * **When to use**
     *
     * Use when a CLI should ask interactively for a missing required flag or
     * argument.
     *
     * **Details**
     *
     * `FallbackPrompt` accepts either a `Prompt` or an effect that builds one.
     * Effectful prompt creation is lazy and runs only when the fallback is needed.
     *
     * **Gotchas**
     *
     * This only handles missing options and missing arguments. Invalid values do not
     * prompt, and prompt cancellation re-fails with the original missing error.
     *
     * @see {@link FallbackPrompt} for accepted fallback prompt forms
     * @see {@link withFallbackConfig} for loading a fallback from config
     * @see {@link withDefault} for a pure default value
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, prompt: FallbackPrompt<B>): Param<Kind, A | B>;
};
/**
 * Represent options which can be used to configure variadic parameters.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export type VariadicParamOptions = {
    /**
     * The minimum number of times the parameter can be specified.
     */
    readonly min?: number | undefined;
    /**
     * The maximum number of times the parameter can be specified.
     */
    readonly max?: number | undefined;
};
/**
 * Creates a variadic parameter that can be specified multiple times.
 *
 * **Details**
 *
 * This is the base combinator for creating parameters that accept multiple values.
 * The `min` and `max` parameters are optional. When they are not provided, the
 * parameter can be specified any number of times, from 0 to infinity.
 *
 * **Example** (Accepting multiple values)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Basic variadic parameter (0 to infinity)
 * const tags = Param.variadic(Param.String(Param.flagKind, "tag"))
 *
 * // Variadic with minimum count
 * const inputs = Param.variadic(
 *   Param.String(Param.flagKind, "input"),
 *   { min: 1 } // at least 1 required
 * )
 *
 * // Variadic with both min and max
 * const limited = Param.variadic(Param.String(Param.flagKind, "item"), {
 *   min: 2, // at least 2 times
 *   max: 2 // at most 2 times
 * })
 * const kinds = [tags.kind, inputs.kind, limited.kind] // => ["flag", "flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const variadic: <Kind extends ParamKind, A>(self: Param<Kind, A>, options?: VariadicParamOptions | undefined) => Param<Kind, ReadonlyArray<A>>;
/**
 * Wraps an option to allow it to be specified multiple times within a range.
 *
 * **Details**
 *
 * This combinator transforms an option to accept between `min` and `max`
 * occurrences on the command line, returning an array of all provided values.
 *
 * **Example** (Bounding repeated values)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Allow 1-3 file inputs
 * const files = Param.String(Param.flagKind, "file").pipe(
 *   Param.between(1, 3),
 *   Param.withAlias("-f")
 * )
 *
 * // Parse: --file a.txt --file b.txt
 * // Result: ["a.txt", "b.txt"]
 *
 * // Allow 0 or more tags
 * const tags = Param.String(Param.flagKind, "tag").pipe(
 *   Param.between(0, Number.MAX_SAFE_INTEGER)
 * )
 *
 * // Parse: --tag dev --tag staging --tag v1.0
 * // Result: ["dev", "staging", "v1.0"]
 * const kinds = [files.kind, tags.kind] // => ["flag", "flag"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const between: {
    /**
     * Wraps an option to allow it to be specified multiple times within a range.
     *
     * **Details**
     *
     * This combinator transforms an option to accept between `min` and `max`
     * occurrences on the command line, returning an array of all provided values.
     *
     * **Example** (Bounding repeated values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Allow 1-3 file inputs
     * const files = Param.String(Param.flagKind, "file").pipe(
     *   Param.between(1, 3),
     *   Param.withAlias("-f")
     * )
     *
     * // Parse: --file a.txt --file b.txt
     * // Result: ["a.txt", "b.txt"]
     *
     * // Allow 0 or more tags
     * const tags = Param.String(Param.flagKind, "tag").pipe(
     *   Param.between(0, Number.MAX_SAFE_INTEGER)
     * )
     *
     * // Parse: --tag dev --tag staging --tag v1.0
     * // Result: ["dev", "staging", "v1.0"]
     * const kinds = [files.kind, tags.kind] // => ["flag", "flag"]
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A>(min: number, max: number): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, ReadonlyArray<A>>;
    /**
     * Wraps an option to allow it to be specified multiple times within a range.
     *
     * **Details**
     *
     * This combinator transforms an option to accept between `min` and `max`
     * occurrences on the command line, returning an array of all provided values.
     *
     * **Example** (Bounding repeated values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Allow 1-3 file inputs
     * const files = Param.String(Param.flagKind, "file").pipe(
     *   Param.between(1, 3),
     *   Param.withAlias("-f")
     * )
     *
     * // Parse: --file a.txt --file b.txt
     * // Result: ["a.txt", "b.txt"]
     *
     * // Allow 0 or more tags
     * const tags = Param.String(Param.flagKind, "tag").pipe(
     *   Param.between(0, Number.MAX_SAFE_INTEGER)
     * )
     *
     * // Parse: --tag dev --tag staging --tag v1.0
     * // Result: ["dev", "staging", "v1.0"]
     * const kinds = [files.kind, tags.kind] // => ["flag", "flag"]
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(self: Param<Kind, A>, min: number, max: number): Param<Kind, ReadonlyArray<A>>;
};
/**
 * Wraps an option to allow it to be specified at most `max` times.
 *
 * **Details**
 *
 * This combinator transforms an option to accept between 0 and `max`
 * occurrences on the command line, returning an array of all provided values.
 *
 * **Example** (Limiting repeated values)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Allow at most 3 warning suppressions
 * const suppressions = Param.String(Param.flagKind, "suppress").pipe(
 *   Param.atMost(3)
 * )
 *
 * // Parse: --suppress warning1 --suppress warning2
 * // Result: ["warning1", "warning2"]
 * suppressions.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const atMost: {
    /**
     * Wraps an option to allow it to be specified at most `max` times.
     *
     * **Details**
     *
     * This combinator transforms an option to accept between 0 and `max`
     * occurrences on the command line, returning an array of all provided values.
     *
     * **Example** (Limiting repeated values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Allow at most 3 warning suppressions
     * const suppressions = Param.String(Param.flagKind, "suppress").pipe(
     *   Param.atMost(3)
     * )
     *
     * // Parse: --suppress warning1 --suppress warning2
     * // Result: ["warning1", "warning2"]
     * suppressions.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A>(max: number): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, ReadonlyArray<A>>;
    /**
     * Wraps an option to allow it to be specified at most `max` times.
     *
     * **Details**
     *
     * This combinator transforms an option to accept between 0 and `max`
     * occurrences on the command line, returning an array of all provided values.
     *
     * **Example** (Limiting repeated values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Allow at most 3 warning suppressions
     * const suppressions = Param.String(Param.flagKind, "suppress").pipe(
     *   Param.atMost(3)
     * )
     *
     * // Parse: --suppress warning1 --suppress warning2
     * // Result: ["warning1", "warning2"]
     * suppressions.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(self: Param<Kind, A>, max: number): Param<Kind, ReadonlyArray<A>>;
};
/**
 * Wraps an option to require it to be specified at least `min` times.
 *
 * **Details**
 *
 * This combinator transforms an option to accept at least `min`
 * occurrences on the command line, returning an array of all provided values.
 *
 * **Example** (Requiring repeated values)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * // Require at least 2 input files
 * const inputs = Param.String(Param.flagKind, "input").pipe(
 *   Param.atLeast(2),
 *   Param.withAlias("-i")
 * )
 *
 * // Parse: --input file1.txt --input file2.txt --input file3.txt
 * // Result: ["file1.txt", "file2.txt", "file3.txt"]
 * inputs.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const atLeast: {
    /**
     * Wraps an option to require it to be specified at least `min` times.
     *
     * **Details**
     *
     * This combinator transforms an option to accept at least `min`
     * occurrences on the command line, returning an array of all provided values.
     *
     * **Example** (Requiring repeated values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Require at least 2 input files
     * const inputs = Param.String(Param.flagKind, "input").pipe(
     *   Param.atLeast(2),
     *   Param.withAlias("-i")
     * )
     *
     * // Parse: --input file1.txt --input file2.txt --input file3.txt
     * // Result: ["file1.txt", "file2.txt", "file3.txt"]
     * inputs.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A>(min: number): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, ReadonlyArray<A>>;
    /**
     * Wraps an option to require it to be specified at least `min` times.
     *
     * **Details**
     *
     * This combinator transforms an option to accept at least `min`
     * occurrences on the command line, returning an array of all provided values.
     *
     * **Example** (Requiring repeated values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * // Require at least 2 input files
     * const inputs = Param.String(Param.flagKind, "input").pipe(
     *   Param.atLeast(2),
     *   Param.withAlias("-i")
     * )
     *
     * // Parse: --input file1.txt --input file2.txt --input file3.txt
     * // Result: ["file1.txt", "file2.txt", "file3.txt"]
     * inputs.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(self: Param<Kind, A>, min: number): Param<Kind, ReadonlyArray<A>>;
};
/**
 * Filters and transforms parsed values, failing with a custom error message
 * if the filter function returns `Option.none()`.
 *
 * **When to use**
 *
 * Use when you need validation and transformation in a single parameter
 * combinator.
 *
 * **Example** (Filtering and transforming values)
 *
 * ```ts import.meta.vitest
 * import { Option } from "effect"
 * import { Param } from "effect/cli"
 * const positiveInt = Param.Int(Param.flagKind, "count").pipe(
 *   Param.filterMap(
 *     (n) => n > 0 ? Option.some(n) : Option.none(),
 *     (n) => `Expected positive integer, got ${n}`
 *   )
 * )
 * positiveInt.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const filterMap: {
    /**
     * Filters and transforms parsed values, failing with a custom error message
     * if the filter function returns `Option.none()`.
     *
     * **When to use**
     *
     * Use when you need validation and transformation in a single parameter
     * combinator.
     *
     * **Example** (Filtering and transforming values)
     *
     * ```ts import.meta.vitest
     * import { Option } from "effect"
     * import { Param } from "effect/cli"
     * const positiveInt = Param.Int(Param.flagKind, "count").pipe(
     *   Param.filterMap(
     *     (n) => n > 0 ? Option.some(n) : Option.none(),
     *     (n) => `Expected positive integer, got ${n}`
     *   )
     * )
     * positiveInt.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A, B>(filter: (a: A) => Option.Option<B>, onNone: (a: A) => string): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, B>;
    /**
     * Filters and transforms parsed values, failing with a custom error message
     * if the filter function returns `Option.none()`.
     *
     * **When to use**
     *
     * Use when you need validation and transformation in a single parameter
     * combinator.
     *
     * **Example** (Filtering and transforming values)
     *
     * ```ts import.meta.vitest
     * import { Option } from "effect"
     * import { Param } from "effect/cli"
     * const positiveInt = Param.Int(Param.flagKind, "count").pipe(
     *   Param.filterMap(
     *     (n) => n > 0 ? Option.some(n) : Option.none(),
     *     (n) => `Expected positive integer, got ${n}`
     *   )
     * )
     * positiveInt.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, filter: (a: A) => Option.Option<B>, onNone: (a: A) => string): Param<Kind, B>;
};
/**
 * Filters parsed values, failing with a custom error message if the predicate returns false.
 *
 * **Example** (Filtering parsed values)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const evenNumber = Param.Int(Param.flagKind, "num").pipe(
 *   Param.filter(
 *     (n) => n % 2 === 0,
 *     (n) => `Expected even number, got ${n}`
 *   )
 * )
 * evenNumber.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const filter: {
    /**
     * Filters parsed values, failing with a custom error message if the predicate returns false.
     *
     * **Example** (Filtering parsed values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const evenNumber = Param.Int(Param.flagKind, "num").pipe(
     *   Param.filter(
     *     (n) => n % 2 === 0,
     *     (n) => `Expected even number, got ${n}`
     *   )
     * )
     * evenNumber.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A>(predicate: Predicate.Predicate<A>, onFalse: (a: A) => string): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, A>;
    /**
     * Filters parsed values, failing with a custom error message if the predicate returns false.
     *
     * **Example** (Filtering parsed values)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const evenNumber = Param.Int(Param.flagKind, "num").pipe(
     *   Param.filter(
     *     (n) => n % 2 === 0,
     *     (n) => `Expected even number, got ${n}`
     *   )
     * )
     * evenNumber.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A>(self: Param<Kind, A>, predicate: Predicate.Predicate<A>, onFalse: (a: A) => string): Param<Kind, A>;
};
/**
 * Sets a custom metavar (placeholder name) for the param in help documentation.
 *
 * **Details**
 *
 * The metavar is displayed in usage text to indicate what value the user should provide.
 * For example, `--output FILE` shows `FILE` as the metavar.
 *
 * **Example** (Setting metavars)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const port = Param.Int(Param.flagKind, "port").pipe(
 *   Param.withMetavar("PORT"),
 *   Param.filter(
 *     (p) => p >= 1 && p <= 65535,
 *     () => "Port must be between 1 and 65535"
 *   )
 * )
 * port.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category metadata
 * @since 4.0.0
 */
export declare const withMetavar: {
    /**
     * Sets a custom metavar (placeholder name) for the param in help documentation.
     *
     * **Details**
     *
     * The metavar is displayed in usage text to indicate what value the user should provide.
     * For example, `--output FILE` shows `FILE` as the metavar.
     *
     * **Example** (Setting metavars)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const port = Param.Int(Param.flagKind, "port").pipe(
     *   Param.withMetavar("PORT"),
     *   Param.filter(
     *     (p) => p >= 1 && p <= 65535,
     *     () => "Port must be between 1 and 65535"
     *   )
     * )
     * port.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category metadata
     * @since 4.0.0
     */
    <K extends ParamKind>(metavar: string): <A>(self: Param<K, A>) => Param<K, A>;
    /**
     * Sets a custom metavar (placeholder name) for the param in help documentation.
     *
     * **Details**
     *
     * The metavar is displayed in usage text to indicate what value the user should provide.
     * For example, `--output FILE` shows `FILE` as the metavar.
     *
     * **Example** (Setting metavars)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const port = Param.Int(Param.flagKind, "port").pipe(
     *   Param.withMetavar("PORT"),
     *   Param.filter(
     *     (p) => p >= 1 && p <= 65535,
     *     () => "Port must be between 1 and 65535"
     *   )
     * )
     * port.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category metadata
     * @since 4.0.0
     */
    <K extends ParamKind, A>(self: Param<K, A>, metavar: string): Param<K, A>;
};
/**
 * Validates parsed values against a Schema, providing detailed error messages.
 *
 * **Example** (Validating with schemas)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { Param } from "effect/cli"
 * const isEmail = Schema.isPattern(/^[^\s@]+@[^\s@]+\.[^\s@]+$/)
 *
 * const Email = Schema.String.pipe(
 *   Schema.check(isEmail)
 * )
 *
 * const email = Param.String(Param.flagKind, "email").pipe(
 *   Param.withSchema(Email)
 * )
 * email.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const withSchema: {
    /**
     * Validates parsed values against a Schema, providing detailed error messages.
     *
     * **Example** (Validating with schemas)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { Param } from "effect/cli"
     * const isEmail = Schema.isPattern(/^[^\s@]+@[^\s@]+\.[^\s@]+$/)
     *
     * const Email = Schema.String.pipe(
     *   Schema.check(isEmail)
     * )
     *
     * const email = Param.String(Param.flagKind, "email").pipe(
     *   Param.withSchema(Email)
     * )
     * email.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <A, B>(schema: Schema.ConstraintCodec<B, A, Environment, unknown>): <Kind extends ParamKind>(self: Param<Kind, A>) => Param<Kind, B>;
    /**
     * Validates parsed values against a Schema, providing detailed error messages.
     *
     * **Example** (Validating with schemas)
     *
     * ```ts import.meta.vitest
     * import { Schema } from "effect"
     * import { Param } from "effect/cli"
     * const isEmail = Schema.isPattern(/^[^\s@]+@[^\s@]+\.[^\s@]+$/)
     *
     * const Email = Schema.String.pipe(
     *   Schema.check(isEmail)
     * )
     *
     * const email = Param.String(Param.flagKind, "email").pipe(
     *   Param.withSchema(Email)
     * )
     * email.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, schema: Schema.ConstraintCodec<B, A, Environment, unknown>): Param<Kind, B>;
};
/**
 * Provides a fallback param to use if this param fails to parse.
 *
 * **Example** (Falling back to another parameter)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const config = Param.File(Param.flagKind, "config").pipe(
 *   Param.orElse(() => Param.String(Param.flagKind, "config-url"))
 * )
 * config.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const orElse: {
    /**
     * Provides a fallback param to use if this param fails to parse.
     *
     * **Example** (Falling back to another parameter)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const config = Param.File(Param.flagKind, "config").pipe(
     *   Param.orElse(() => Param.String(Param.flagKind, "config-url"))
     * )
     * config.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <B, Kind extends ParamKind>(orElse: LazyArg<Param<Kind, B>>): <A>(self: Param<Kind, A>) => Param<Kind, A | B>;
    /**
     * Provides a fallback param to use if this param fails to parse.
     *
     * **Example** (Falling back to another parameter)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const config = Param.File(Param.flagKind, "config").pipe(
     *   Param.orElse(() => Param.String(Param.flagKind, "config-url"))
     * )
     * config.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, orElse: LazyArg<Param<Kind, B>>): Param<Kind, A | B>;
};
/**
 * Provides a fallback param and returns a `Result` indicating which param
 * succeeded.
 *
 * **Details**
 *
 * The original param's value is returned as `Result.succeed`, while the
 * fallback param's value is returned as `Result.fail`.
 *
 * **Example** (Returning fallback results)
 *
 * ```ts import.meta.vitest
 * import { Param } from "effect/cli"
 *
 * const configSource = Param.File(Param.flagKind, "config").pipe(
 *   Param.orElseResult(() => Param.String(Param.flagKind, "config-url"))
 * )
 * // Returns Result<string, string>
 * configSource.kind // => "flag"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const orElseResult: {
    /**
     * Provides a fallback param and returns a `Result` indicating which param
     * succeeded.
     *
     * **Details**
     *
     * The original param's value is returned as `Result.succeed`, while the
     * fallback param's value is returned as `Result.fail`.
     *
     * **Example** (Returning fallback results)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const configSource = Param.File(Param.flagKind, "config").pipe(
     *   Param.orElseResult(() => Param.String(Param.flagKind, "config-url"))
     * )
     * // Returns Result<string, string>
     * configSource.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, B>(orElse: LazyArg<Param<Kind, B>>): <A>(self: Param<Kind, A>) => Param<Kind, Result.Result<A, B>>;
    /**
     * Provides a fallback param and returns a `Result` indicating which param
     * succeeded.
     *
     * **Details**
     *
     * The original param's value is returned as `Result.succeed`, while the
     * fallback param's value is returned as `Result.fail`.
     *
     * **Example** (Returning fallback results)
     *
     * ```ts import.meta.vitest
     * import { Param } from "effect/cli"
     *
     * const configSource = Param.File(Param.flagKind, "config").pipe(
     *   Param.orElseResult(() => Param.String(Param.flagKind, "config-url"))
     * )
     * // Returns Result<string, string>
     * configSource.kind // => "flag"
     * ```
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Kind extends ParamKind, A, B>(self: Param<Kind, A>, orElse: LazyArg<Param<Kind, B>>): Param<Kind, Result.Result<A, B>>;
};
export {};
//# sourceMappingURL=Param.d.ts.map