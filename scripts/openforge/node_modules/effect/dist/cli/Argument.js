import { dual } from "../Function.js";
import * as Param from "./Param.js";
// -------------------------------------------------------------------------------------
// constructors
// -------------------------------------------------------------------------------------
/**
 * Creates a positional string argument.
 *
 * **Example** (Creating a string argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const filename = Argument.String("filename")
 * filename.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const String = name => Param.String(Param.argumentKind, name);
/**
 * Creates a positional integer argument.
 *
 * **Example** (Creating an integer argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const count = Argument.Int("count")
 * count.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Int = name => Param.Int(Param.argumentKind, name);
/**
 * Creates a positional file path argument.
 *
 * **Example** (Creating file path arguments)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const inputFile = Argument.File("input", { mustExist: true }) // Must exist
 * const outputFile = Argument.File("output", { mustExist: false }) // Must not exist
 * const kinds = [inputFile.kind, outputFile.kind] // => ["argument", "argument"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const File = (name, options) => Param.File(Param.argumentKind, name, options);
/**
 * Creates a positional directory path argument.
 *
 * **Example** (Creating a directory path argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const workspace = Argument.Directory("workspace", { mustExist: true }) // Must exist
 * workspace.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Directory = (name, options) => Param.Directory(Param.argumentKind, name, options);
/**
 * Creates a positional argument that parses finite numbers.
 *
 * **Example** (Parsing a finite number)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const ratio = Argument.Finite("ratio")
 * ratio.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Finite = name => Param.Finite(Param.argumentKind, name);
/**
 * Creates a positional date argument.
 *
 * **Example** (Creating a date argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const startDate = Argument.Date("start-date")
 * startDate.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Date = name => Param.Date(Param.argumentKind, name);
/**
 * Creates a positional choice argument.
 *
 * **Example** (Creating a choice argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const environment = Argument.Literals("environment", ["dev", "staging", "prod"])
 * environment.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Literals = (name, choices) => Param.Literals(Param.argumentKind, name, choices);
/**
 * Creates a positional path argument.
 *
 * **Example** (Creating a path argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const configPath = Argument.Path("config")
 * configPath.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Path = (name, options) => Param.Path(Param.argumentKind, name, options);
/**
 * Creates a positional redacted argument that obscures its value.
 *
 * **Example** (Creating a redacted argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const secret = Argument.Redacted("secret")
 * secret.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Redacted = name => Param.Redacted(Param.argumentKind, name);
/**
 * Creates a positional argument that reads file content as a string.
 *
 * **Example** (Reading file text)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const config = Argument.FileText("config-file")
 * config.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const FileText = name => Param.FileText(Param.argumentKind, name);
/**
 * Creates a positional argument that reads a file and parses its content.
 *
 * **Details**
 *
 * The parser is chosen from the explicit `format` option or, when omitted, the
 * file extension. The parsed value is `unknown`; use `fileSchema` when the
 * parsed content should also be decoded with a Schema.
 *
 * **Example** (Parsing file content)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const config = Argument.FileParse("config", { format: "json" })
 * config.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const FileParse = (name, options) => Param.FileParse(Param.argumentKind, name, options);
/**
 * Creates a positional argument that reads and validates file content using a schema.
 *
 * **Example** (Validating file content with a schema)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { Argument } from "effect/cli"
 *
 * const ConfigSchema = Schema.Struct({
 *   port: Schema.Number,
 *   host: Schema.String
 * })
 *
 * const config = Argument.FileSchema("config", ConfigSchema)
 * config.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const FileSchema = (name, schema, options) => Param.FileSchema(Param.argumentKind, name, schema, options);
/**
 * An argument that always fails to parse.
 *
 * **Example** (Creating a sentinel argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const noArg = Argument.Never
 * noArg.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Never = /*#__PURE__*/Param.Never(Param.argumentKind);
// -------------------------------------------------------------------------------------
// combinators
// -------------------------------------------------------------------------------------
/**
 * Makes a positional argument optional.
 *
 * **Example** (Making an argument optional)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const optionalVersion = Argument.String("version").pipe(Argument.optional)
 * optionalVersion.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const optional = arg => Param.optional(arg);
/**
 * Adds a description to a positional argument.
 *
 * **Example** (Adding an argument description)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const filename = Argument.String("filename").pipe(
 *   Argument.withDescription("The input file to process")
 * )
 * filename.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withDescription = /*#__PURE__*/dual(2, (self, description) => Param.withDescription(self, description));
/**
 * Provides a default value for a positional argument.
 *
 * **Example** (Providing a default value)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const port = Argument.Int("port").pipe(Argument.withDefault(8080))
 * port.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withDefault = Param.withDefault;
/**
 * Adds a fallback config that is loaded when a required argument is missing.
 *
 * **Example** (Loading a fallback config)
 *
 * ```ts import.meta.vitest
 * import { Config } from "effect"
 * import { Argument } from "effect/cli"
 *
 * const repository = Argument.String("repository").pipe(
 *   Argument.withFallbackConfig(Config.String("REPOSITORY"))
 * )
 * repository.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withFallbackConfig = /*#__PURE__*/dual(2, (self, config) => Param.withFallbackConfig(self, config));
/**
 * Adds a fallback prompt that is shown when a required argument is missing.
 *
 * **Example** (Showing a fallback prompt)
 *
 * ```ts import.meta.vitest
 * import { Argument, Prompt } from "effect/cli"
 *
 * const filename = Argument.String("filename").pipe(
 *   Argument.withFallbackPrompt(Prompt.String({ message: "Filename" }))
 * )
 * filename.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withFallbackPrompt = /*#__PURE__*/dual(2, (self, prompt) => Param.withFallbackPrompt(self, prompt));
/**
 * Creates a variadic positional argument that accepts multiple values.
 *
 * **Example** (Accepting multiple values)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * // Accept any number of files
 * const anyFiles = Argument.String("files").pipe(Argument.variadic)
 *
 * // Accept at least 1 file
 * const atLeastOneFile = Argument.String("files").pipe(
 *   Argument.variadic({ min: 1 })
 * )
 *
 * // Accept between 1 and 5 files
 * const limitedFiles = Argument.String("files").pipe(
 *   Argument.variadic({ min: 1, max: 5 })
 * )
 *
 * const kinds = [anyFiles.kind, atLeastOneFile.kind, limitedFiles.kind] // => ["argument", "argument", "argument"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const variadic = /*#__PURE__*/dual(args => Param.isParam(args[0]), (self, options) => Param.variadic(self, options));
/**
 * Transforms the parsed value of a positional argument.
 *
 * **Example** (Mapping parsed values)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const port = Argument.Int("port").pipe(
 *   Argument.map((p) => ({ port: p, url: `http://localhost:${p}` }))
 * )
 * port.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const map = /*#__PURE__*/dual(2, (self, f) => Param.map(self, f));
/**
 * Transforms the parsed value of a positional argument using an effectful function.
 *
 * **Example** (Validating values effectfully)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Argument, CliError } from "effect/cli"
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
 * const files = Argument.String("files").pipe(
 *   Argument.mapEffect((file) =>
 *     file.endsWith(".txt")
 *       ? Effect.succeed(file)
 *       : Effect.fail(
 *         new CliError.UserError({
 *           cause: new Error(`Unsupported file extension: ${file}`),
 *           userMessage: "Only .txt files allowed"
 *         })
 *       )
 *   )
 * )
 *
 * const [, value] = await Effect.runPromise(
 *   files.parse({ arguments: ["notes.txt"], flags: {} }).pipe(Effect.provide(CliTestLayer))
 * )
 * value // => "notes.txt"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const mapEffect = /*#__PURE__*/dual(2, (self, f) => Param.mapEffect(self, f));
/**
 * Transforms the parsed value of a positional argument using a function that may throw.
 *
 * **Example** (Mapping values that may throw)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Argument } from "effect/cli"
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
 * const json = Argument.String("data").pipe(
 *   Argument.mapTryCatch(
 *     (str) => JSON.parse(str),
 *     (error) =>
 *       `Invalid JSON: ${error instanceof Error ? error.message : String(error)}`
 *   )
 * )
 *
 * const [, value] = await Effect.runPromise(
 *   json.parse({ arguments: ['{"enabled":true}'], flags: {} }).pipe(Effect.provide(CliTestLayer))
 * )
 * value // => { enabled: true }
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const mapTryCatch = /*#__PURE__*/dual(3, (self, f, onError) => Param.mapTryCatch(self, f, onError));
/**
 * Creates a variadic argument that requires at least n values.
 *
 * **Example** (Requiring a minimum number of values)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const files = Argument.String("files").pipe(Argument.atLeast(1))
 * files.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const atLeast = /*#__PURE__*/dual(2, (self, min) => Param.atLeast(self, min));
/**
 * Creates a variadic argument that accepts at most n values.
 *
 * **Example** (Limiting the maximum number of values)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const files = Argument.String("files").pipe(Argument.atMost(5))
 * files.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const atMost = /*#__PURE__*/dual(2, (self, max) => Param.atMost(self, max));
/**
 * Creates a variadic argument that accepts between min and max values.
 *
 * **Example** (Requiring a range of values)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const files = Argument.String("files").pipe(Argument.between(1, 5))
 * files.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const between = /*#__PURE__*/dual(3, (self, min, max) => Param.between(self, min, max));
/**
 * Validates parsed values against a Schema.
 *
 * **Example** (Validating parsed values with a schema)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { Argument } from "effect/cli"
 *
 * const input = Argument.String("input").pipe(
 *   Argument.withSchema(Schema.NonEmptyString)
 * )
 * input.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withSchema = /*#__PURE__*/dual(2, (self, schema) => Param.withSchema(self, schema));
/**
 * Creates a positional choice argument with custom value mapping.
 *
 * **Example** (Mapping choices to values)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const logLevel = Argument.ChoiceWithValue("level", [
 *   ["debug", 0],
 *   ["info", 1],
 *   ["warn", 2],
 *   ["error", 3]
 * ])
 * logLevel.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const ChoiceWithValue = (name, choices) => Param.ChoiceWithValue(Param.argumentKind, name, choices);
// -------------------------------------------------------------------------------------
// metadata
// -------------------------------------------------------------------------------------
/**
 * Sets a custom metavar (placeholder name) for the argument in help documentation.
 *
 * **Details**
 *
 * The metavar is displayed in usage text to indicate what value the user should provide.
 * For example, `<FILE>` shows `FILE` as the metavar.
 *
 * **Example** (Setting a metavar)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const port = Argument.Int("port").pipe(
 *   Argument.withMetavar("PORT")
 * )
 * port.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category metadata
 * @since 4.0.0
 */
export const withMetavar = /*#__PURE__*/dual(2, (self, metavar) => Param.withMetavar(self, metavar));
/**
 * Filters parsed values, failing with a custom error message if the predicate returns false.
 *
 * **Example** (Filtering parsed values)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const positiveInt = Argument.Int("count").pipe(
 *   Argument.filter(
 *     (n) => n > 0,
 *     (n) => `Expected positive integer, got ${n}`
 *   )
 * )
 * positiveInt.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const filter = /*#__PURE__*/dual(3, (self, predicate, onFalse) => Param.filter(self, predicate, onFalse));
/**
 * Filters and transforms parsed values, failing with a custom error message
 * if the filter function returns None.
 *
 * **Example** (Filtering and mapping parsed values)
 *
 * ```ts import.meta.vitest
 * import { Option } from "effect"
 * import { Argument } from "effect/cli"
 *
 * const positiveInt = Argument.Int("count").pipe(
 *   Argument.filterMap(
 *     (n) => n > 0 ? Option.some(n) : Option.none(),
 *     (n) => `Expected positive integer, got ${n}`
 *   )
 * )
 * positiveInt.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const filterMap = /*#__PURE__*/dual(3, (self, f, onNone) => Param.filterMap(self, f, onNone));
/**
 * Provides a fallback argument to use if this argument fails to parse.
 *
 * **Example** (Providing a fallback argument)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const value = Argument.Int("value").pipe(
 *   Argument.orElse(() => Argument.String("value"))
 * )
 * value.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const orElse = /*#__PURE__*/dual(2, (self, that) => Param.orElse(self, that));
/**
 * Provides a fallback argument, wrapping results in Result to distinguish which succeeded.
 *
 * **Example** (Returning which fallback succeeded)
 *
 * ```ts import.meta.vitest
 * import { Argument } from "effect/cli"
 *
 * const source = Argument.File("source").pipe(
 *   Argument.orElseResult(() => Argument.String("url"))
 * )
 * // Returns Result<string, string>
 * source.kind // => "argument"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const orElseResult = /*#__PURE__*/dual(2, (self, that) => Param.orElseResult(self, that));
//# sourceMappingURL=Argument.js.map