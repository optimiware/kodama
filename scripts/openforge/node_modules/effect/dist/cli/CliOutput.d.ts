/**
 * Formats CLI help and errors as text.
 *
 * This module turns help documents, CLI errors, grouped errors, and version
 * information into strings. It does not write those strings to the terminal
 * itself. It includes the `Formatter` interface, the formatter service, a layer
 * for custom formatters, and the default formatter with configurable color
 * support.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.ts";
import * as Layer from "../Layer.ts";
import type * as CliError from "./CliError.ts";
import type { HelpDoc } from "./HelpDoc.ts";
/**
 * Defines the service interface for formatting CLI output including help, errors, and version info.
 * This allows customization of output formatting, including color support.
 *
 * **Example** (Customizing CLI output formatting)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliOutput } from "effect/cli"
 *
 * // Create a custom formatter implementation
 * const customFormatter: CliOutput.Formatter = {
 *   formatHelpDoc: (doc) => `Custom Help: ${doc.usage}`,
 *   formatCliError: (error) => `Error: ${error.message}`,
 *   formatError: (error) => `[ERROR] ${error.message}`,
 *   formatVersion: (name, version) => `${name} (${version})`,
 *   formatErrors: (errors) => errors.map((error) => error.message).join("\\n")
 * }
 *
 * // Use the custom formatter in a program
 * const program = Effect.gen(function*() {
 *   const formatter = yield* CliOutput.Formatter
 *   return formatter.formatVersion("myapp", "1.0.0")
 * }).pipe(
 *   Effect.provide(CliOutput.layer(customFormatter))
 * )
 *
 * await Effect.runPromise(program) // => "myapp (1.0.0)"
 * ```
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Formatter {
    /**
     * Formats a HelpDoc structure into a readable string format.
     *
     * **Example** (Formatting help documents)
     *
     * ```ts import.meta.vitest
     * import { Option as O } from "effect"
     * import { CliOutput } from "effect/cli"
     * import type { HelpDoc } from "effect/cli"
     *
     * const helpDoc: HelpDoc = {
     *   usage: "myapp [options] <file>",
     *   description: "Process files with various options",
     *   flags: [
     *     {
     *       name: "verbose",
     *       aliases: ["-v"],
     *       type: "boolean",
     *       description: O.some("Enable verbose output"),
     *       required: false
     *     }
     *   ],
     *   args: [
     *     {
     *       name: "file",
     *       type: "string",
     *       description: O.some("Input file to process"),
     *       required: true,
     *       variadic: false
     *     }
     *   ]
     * }
     *
     * const formatter = CliOutput.defaultFormatter()
     * const helpText = formatter.formatHelpDoc(helpDoc)
     * const sectionsPresent = [helpText.includes("DESCRIPTION"), helpText.includes("FLAGS")] // => [true, true]
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly formatHelpDoc: (doc: HelpDoc) => string;
    /**
     * Formats a CLI error for display. Default implementation mirrors the error message.
     *
     * **Example** (Formatting CLI errors)
     *
     * ```ts import.meta.vitest
     * import { Data } from "effect"
     * import { CliOutput } from "effect/cli"
     *
     * class InvalidOption extends Data.TaggedError("InvalidOption")<{
     *   readonly message: string
     * }> {}
     *
     * const formatter = CliOutput.defaultFormatter()
     * const error = new InvalidOption({ message: "Unknown flag '--invalid'" })
     * const errorMessage = formatter.formatCliError(error)
     * errorMessage // => "Unknown flag '--invalid'"
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly formatCliError: (error: CliError.CliError) => string;
    /**
     * Formats an error section with proper styling and color reset.
     *
     * **Example** (Formatting error sections)
     *
     * ```ts import.meta.vitest
     * import { Data } from "effect"
     * import { CliOutput } from "effect/cli"
     *
     * class ValidationError extends Data.TaggedError("ValidationError")<{
     *   readonly message: string
     * }> {}
     *
     * const colorFormatter = CliOutput.defaultFormatter({ colors: true })
     * const noColorFormatter = CliOutput.defaultFormatter({ colors: false })
     *
     * const error = new ValidationError({ message: "Value must be positive" })
     *
     * const coloredError = colorFormatter.formatError(error)
     * coloredError.includes("\u001b[31mERROR") // => true
     *
     * const plainError = noColorFormatter.formatError(error)
     * plainError // => "\nERROR\n  Value must be positive"
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly formatError: (error: CliError.CliError) => string;
    /**
     * Formats version output for display.
     *
     * **Example** (Formatting version output)
     *
     * ```ts import.meta.vitest
     * import { CliOutput } from "effect/cli"
     *
     * const colorFormatter = CliOutput.defaultFormatter({ colors: true })
     * const noColorFormatter = CliOutput.defaultFormatter({ colors: false })
     *
     * const appName = "my-awesome-tool"
     * const version = "1.2.3"
     *
     * const coloredVersion = colorFormatter.formatVersion(appName, version)
     * coloredVersion // => "\u001b[1mmy-awesome-tool\u001b[0m \u001b[2mv\u001b[0m\u001b[1m1.2.3\u001b[0m"
     *
     * const plainVersion = noColorFormatter.formatVersion(appName, version)
     * plainVersion // => "my-awesome-tool v1.2.3"
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly formatVersion: (name: string, version: string) => string;
    /**
     * Formats multiple CLI errors for display, grouping by error type.
     *
     * **Example** (Formatting grouped errors)
     *
     * ```ts import.meta.vitest
     * import { CliError, CliOutput } from "effect/cli"
     *
     * const formatter = CliOutput.defaultFormatter({ colors: false })
     *
     * const errors = [
     *   new CliError.UnrecognizedOption({
     *     option: "--foo",
     *     suggestions: ["--force"]
     *   }),
     *   new CliError.UnrecognizedOption({ option: "--bar", suggestions: [] }),
     *   new CliError.MissingOption({ option: "--required" })
     * ]
     *
     * const output = formatter.formatErrors(errors)
     * const optionsPresent = [output.includes("--foo"), output.includes("--required")] // => [true, true]
     * ```
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly formatErrors: (errors: ReadonlyArray<CliError.CliError>) => string;
}
/**
 * Service reference for the CLI output formatter. Provides a default implementation
 * that can be overridden for custom formatting or testing.
 *
 * **Example** (Accessing the output formatter)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliOutput } from "effect/cli"
 *
 * // Access the formatter service
 * const program = Effect.gen(function*() {
 *   const formatter = yield* CliOutput.Formatter
 *
 *   // Format version information
 *   return formatter.formatVersion("my-cli", "2.1.0")
 * })
 *
 * // Run with default formatter
 * await Effect.runPromise(program.pipe(
 *   Effect.provide(CliOutput.layer(CliOutput.defaultFormatter({ colors: false })))
 * )) // => "my-cli v2.1.0"
 * ```
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const Formatter: Context.Reference<Formatter>;
/**
 * Creates a Layer that provides a custom Formatter implementation.
 *
 * **Example** (Providing a custom formatter)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliOutput } from "effect/cli"
 *
 * // Create a custom formatter without colors
 * const noColorFormatter = CliOutput.defaultFormatter({ colors: false })
 * const NoColorLayer = CliOutput.layer(noColorFormatter)
 *
 * // Create a program that uses the custom formatter
 * const program = Effect.gen(function*() {
 *   const formatter = yield* CliOutput.Formatter
 *   return formatter.formatVersion("my-cli", "1.0.0")
 * }).pipe(
 *   Effect.provide(NoColorLayer)
 * )
 *
 * await Effect.runPromise(program) // => "my-cli v1.0.0"
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: (formatter: Formatter) => Layer.Layer<never>;
/**
 * Creates a default formatter with configurable options.
 *
 * **Example** (Creating default formatters)
 *
 * ```ts import.meta.vitest
 * import { CliError, CliOutput } from "effect/cli"
 *
 * // Create a formatter without colors for tests or CI environments
 * const noColorFormatter = CliOutput.defaultFormatter({ colors: false })
 *
 * // Create a formatter with colors forced on
 * const colorFormatter = CliOutput.defaultFormatter({ colors: true })
 *
 * // Auto-detect colors based on terminal support (default behavior)
 * const autoFormatter = CliOutput.defaultFormatter()
 *
 * const error = new CliError.InvalidValue({
 *   option: "foo",
 *   value: "bar",
 *   expected: "baz",
 *   kind: "flag"
 * })
 *
 * noColorFormatter.formatError(error).includes("Invalid value") // => true
 * colorFormatter.formatVersion("my-tool", "1.2.3").includes("my-tool") // => true
 * autoFormatter.formatVersion("my-tool", "1.2.3").includes("1.2.3") // => true
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const defaultFormatter: (options?: {
    colors?: boolean;
}) => Formatter;
//# sourceMappingURL=CliOutput.d.ts.map