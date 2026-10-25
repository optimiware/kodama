import * as Runtime from "../Runtime.ts";
import * as Schema from "../Schema.ts";
/**
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
declare const TypeId = "~effect/cli/CliError";
/**
 * Type guard to check if a value is a CLI error.
 *
 * **Example** (Checking CLI errors)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliError } from "effect/cli"
 *
 * const error = new CliError.MissingOption({ option: "api-key" })
 * const program = CliError.isCliError(error)
 *   ? Effect.succeed(error.message)
 *   : Effect.fail("Unknown error")
 *
 * await Effect.runPromise(program) // => "Missing required flag: --api-key"
 * ```
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isCliError: (u: unknown) => u is CliError;
/**
 * Union type representing all possible CLI error conditions.
 *
 * **Example** (Handling CLI errors)
 *
 * ```ts import.meta.vitest
 * import { CliError } from "effect/cli"
 *
 * const describe = (error: CliError.CliError): string => {
 *   switch (error._tag) {
 *     case "UnrecognizedOption":
 *       return `Unknown flag: ${error.option}`
 *     case "MissingOption":
 *       return `Required flag missing: ${error.option}`
 *     case "InvalidValue":
 *       return `Invalid value: ${error.value} for ${error.option}`
 *     case "ShowHelp":
 *       return `Help requested for: ${error.commandPath.join(" ")}`
 *     default:
 *       return error.message
 *   }
 * }
 *
 * describe(new CliError.MissingOption({ option: "token" })) // => "Required flag missing: token"
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type CliError = UnrecognizedOption | DuplicateOption | MissingOption | MissingArgument | UnexpectedArgument | InvalidValue | UnknownSubcommand | ShowHelp | UserError;
declare const UnrecognizedOption_base: Schema.Class<UnrecognizedOption, Schema.TaggedStruct<"UnrecognizedOption", {
    readonly option: Schema.String;
    readonly command: Schema.optional<Schema.$Array<Schema.String>>;
    readonly suggestions: Schema.$Array<Schema.String>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error thrown when an unrecognized option is encountered.
 *
 * **Example** (Creating unrecognized option errors)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliError } from "effect/cli"
 *
 * // Creating an unrecognized option error
 * const unrecognizedError = new CliError.UnrecognizedOption({
 *   option: "--unknown-flag",
 *   command: ["deploy", "production"],
 *   suggestions: ["--verbose", "--force"]
 * })
 *
 * unrecognizedError._tag // => "UnrecognizedOption"
 * unrecognizedError.option // => "--unknown-flag"
 * unrecognizedError.command // => ["deploy", "production"]
 *
 * // In CLI parsing context
 * const parseCommand = Effect.gen(function*() {
 *   // If parsing encounters unknown flag
 *   return yield* unrecognizedError
 * })
 *
 * const parseError = await Effect.runPromise(Effect.flip(parseCommand))
 * parseError._tag // => "UnrecognizedOption"
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class UnrecognizedOption extends UnrecognizedOption_base {
    /**
     * Marks this value as a CLI parsing error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Formats the unrecognized option with command context and suggestions.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const DuplicateOption_base: Schema.Class<DuplicateOption, Schema.TaggedStruct<"DuplicateOption", {
    readonly option: Schema.String;
    readonly parentCommand: Schema.String;
    readonly childCommand: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error thrown when duplicate option names are detected between parent and child commands.
 *
 * **Example** (Creating duplicate option errors)
 *
 * ```ts import.meta.vitest
 * import { CliError } from "effect/cli"
 *
 * const duplicateError = new CliError.DuplicateOption({
 *   option: "--verbose",
 *   parentCommand: "myapp",
 *   childCommand: "deploy"
 * })
 *
 * duplicateError._tag // => "DuplicateOption"
 * duplicateError.option // => "--verbose"
 * duplicateError.parentCommand // => "myapp"
 * duplicateError.childCommand // => "deploy"
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class DuplicateOption extends DuplicateOption_base {
    /**
     * Marks this value as a CLI configuration error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Explains which parent and child commands define the duplicate option.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const MissingOption_base: Schema.Class<MissingOption, Schema.TaggedStruct<"MissingOption", {
    readonly option: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error thrown when a required option is missing.
 *
 * **Example** (Creating missing option errors)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliError } from "effect/cli"
 *
 * const missingOptionError = new CliError.MissingOption({
 *   option: "api-key"
 * })
 *
 * const details = [missingOptionError._tag, missingOptionError.option] // => ["MissingOption", "api-key"]
 *
 * // In validation context
 * const validateRequiredOptions = (options: Record<string, string | undefined>) =>
 *   Effect.gen(function*() {
 *     const apiKey = options["api-key"]
 *     if (!apiKey) {
 *       return yield* missingOptionError
 *     }
 *     return apiKey
 *   })
 *
 * const validationError = await Effect.runPromise(Effect.flip(validateRequiredOptions({})))
 * validationError._tag // => "MissingOption"
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class MissingOption extends MissingOption_base {
    /**
     * Marks this value as a missing CLI option error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Formats the missing required flag for display.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const MissingArgument_base: Schema.Class<MissingArgument, Schema.TaggedStruct<"MissingArgument", {
    readonly argument: Schema.String;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error thrown when a required positional argument is missing.
 *
 * **Example** (Creating missing argument errors)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliError } from "effect/cli"
 *
 * const missingArgError = new CliError.MissingArgument({
 *   argument: "target"
 * })
 *
 * const details = [missingArgError._tag, missingArgError.argument] // => ["MissingArgument", "target"]
 *
 * // In argument parsing
 * const parseArguments = (args: Array<string>) =>
 *   Effect.gen(function*() {
 *     if (args.length === 0) {
 *       return yield* missingArgError
 *     }
 *     return args[0]
 *   })
 *
 * const parseError = await Effect.runPromise(Effect.flip(parseArguments([])))
 * parseError._tag // => "MissingArgument"
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class MissingArgument extends MissingArgument_base {
    /**
     * Marks this value as a missing CLI argument error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Formats the missing required positional argument for display.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const UnexpectedArgument_base: Schema.Class<UnexpectedArgument, Schema.TaggedStruct<"UnexpectedArgument", {
    readonly arguments: Schema.$Array<Schema.String>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error thrown when positional arguments remain after a command has parsed all
 * of its parameters.
 *
 * **Example** (Reporting unexpected arguments)
 *
 * ```ts import.meta.vitest
 * import { CliError } from "effect/cli"
 *
 * const error = new CliError.UnexpectedArgument({
 *   arguments: ["extra.txt"]
 * })
 *
 * const details = [error._tag, error.arguments] // => ["UnexpectedArgument", ["extra.txt"]]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class UnexpectedArgument extends UnexpectedArgument_base {
    /**
     * Marks this value as an unexpected CLI argument error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Formats the unexpected positional arguments for display.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const InvalidValue_base: Schema.Class<InvalidValue, Schema.TaggedStruct<"InvalidValue", {
    readonly option: Schema.String;
    readonly value: Schema.String;
    readonly expected: Schema.String;
    readonly kind: Schema.Union<readonly [Schema.Literal<"flag">, Schema.Literal<"argument">]>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error thrown when an option or argument value is invalid.
 *
 * **Example** (Creating invalid value errors)
 *
 * ```ts import.meta.vitest
 * import { CliError } from "effect/cli"
 *
 * const invalidValueError = new CliError.InvalidValue({
 *   option: "port",
 *   value: "abc123",
 *   expected: "integer between 1 and 65535",
 *   kind: "flag"
 * })
 *
 * invalidValueError._tag // => "InvalidValue"
 * invalidValueError.kind // => "flag"
 * invalidValueError.option // => "port"
 * invalidValueError.value // => "abc123"
 *
 * // For positional arguments
 * const invalidArgError = new CliError.InvalidValue({
 *   option: "count",
 *   value: "abc",
 *   expected: "integer",
 *   kind: "argument"
 * })
 *
 * const details = [invalidArgError.kind, invalidArgError.option, invalidArgError.value] // => ["argument", "count", "abc"]
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidValue extends InvalidValue_base {
    /**
     * Marks this value as an invalid CLI value error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Formats the invalid flag or argument value with the expected input.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const UnknownSubcommand_base: Schema.Class<UnknownSubcommand, Schema.TaggedStruct<"UnknownSubcommand", {
    readonly subcommand: Schema.String;
    readonly parent: Schema.optional<Schema.$Array<Schema.String>>;
    readonly suggestions: Schema.$Array<Schema.String>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error thrown when an unknown subcommand is encountered.
 *
 * **Example** (Creating unknown subcommand errors)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliError } from "effect/cli"
 *
 * const unknownSubcommandError = new CliError.UnknownSubcommand({
 *   subcommand: "deplyo", // typo
 *   parent: ["myapp"],
 *   suggestions: ["deploy", "destroy"]
 * })
 *
 * unknownSubcommandError._tag // => "UnknownSubcommand"
 * unknownSubcommandError.subcommand // => "deplyo"
 * unknownSubcommandError.parent // => ["myapp"]
 *
 * // In subcommand parsing
 * const parseSubcommand = (subcommand: string) =>
 *   Effect.gen(function*() {
 *     const validCommands = ["deploy", "destroy", "status"]
 *     if (!validCommands.includes(subcommand)) {
 *       return yield* unknownSubcommandError
 *     }
 *     return subcommand
 *   })
 *
 * const parseError = await Effect.runPromise(Effect.flip(parseSubcommand("deplyo")))
 * parseError._tag // => "UnknownSubcommand"
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class UnknownSubcommand extends UnknownSubcommand_base {
    /**
     * Marks this value as an unknown CLI subcommand error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Formats the unknown subcommand with parent command context and suggestions.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const UserError_base: Schema.Class<UserError, Schema.TaggedStruct<"UserError", {
    readonly cause: Schema.Defect;
    readonly userMessage: Schema.optionalKey<Schema.String>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error wrapper for user handler failures in the CLI error channel.
 *
 * **Details**
 *
 * `userMessage` can provide safe, user-facing text independently of the
 * underlying cause. When omitted or empty, `message` uses a non-empty string
 * cause or `Error.message`, then falls back to `"An error occurred"`.
 *
 * **Example** (Wrapping user errors)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { CliError } from "effect/cli"
 *
 * // Wrapping user errors
 * const userError = new CliError.UserError({
 *   cause: new Error("Database connection failed for postgres://localhost"),
 *   userMessage: "Could not connect to the database"
 * })
 *
 * // In command handler
 * const deployCommand = Effect.gen(function*() {
 *   const result = yield* Effect.try({
 *     try: () => ({ deployed: true }),
 *     catch: (error) => new CliError.UserError({ cause: error })
 *   })
 *   return result
 * })
 *
 * // In error handling
 * const handleError = (error: CliError.CliError): Effect.Effect<number> => {
 *   if (error._tag === "UserError") {
 *     return Effect.succeed(1) // Exit code 1
 *   }
 *   return Effect.succeed(0)
 * }
 *
 * await Effect.runPromise(deployCommand) // => { deployed: true }
 * await Effect.runPromise(handleError(userError)) // => 1
 * ```
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class UserError extends UserError_base {
    /**
     * Marks this value as a user handler error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId] = "~effect/cli/CliError";
    /**
     * Controls whether the runtime logger should report this error. The CLI
     * runner sets this to `false` after rendering the error itself.
     *
     * @stability unstable
     * @since 4.0.0
     */
    [Runtime.errorReported]: boolean;
    /**
     * Returns the explicit user-facing message or a safe fallback from `cause`.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
/**
 * Schema for concrete CLI errors that can be reported together with help output.
 *
 * **Details**
 *
 * This excludes `ShowHelp` itself, allowing parse and validation errors to be
 * stored in `ShowHelp.errors` without nesting another help-control value.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const NonShowHelpErrors: Schema.Union<readonly [
    typeof UnrecognizedOption,
    typeof DuplicateOption,
    typeof MissingOption,
    typeof MissingArgument,
    typeof UnexpectedArgument,
    typeof InvalidValue,
    typeof UnknownSubcommand,
    typeof UserError
]>;
/**
 * Type of CLI errors that are not `ShowHelp`.
 *
 * **Details**
 *
 * These errors can be accumulated and attached to `ShowHelp.errors` when the
 * runner should display help along with the underlying parse or validation
 * failures.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type NonShowHelpErrors = typeof NonShowHelpErrors.Type;
declare const ShowHelp_base: Schema.Class<ShowHelp, Schema.TaggedStruct<"ShowHelp", {
    readonly commandPath: Schema.$Array<Schema.String>;
    readonly errors: Schema.$Array<Schema.Union<readonly [typeof UnrecognizedOption, typeof DuplicateOption, typeof MissingOption, typeof MissingArgument, typeof UnexpectedArgument, typeof InvalidValue, typeof UnknownSubcommand, typeof UserError]>>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error data requesting CLI help rendering for a command path.
 *
 * **Details**
 *
 * It is used for explicit help requests and for parse or validation failures
 * that should be shown with help text. When `errors` is non-empty, the runtime
 * exit code is `1`; otherwise it is `0`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ShowHelp extends ShowHelp_base {
    readonly [TypeId] = "~effect/cli/CliError";
    readonly [Runtime.errorExitCode]: number;
    readonly [Runtime.errorReported] = false;
    get message(): string;
}
export {};
//# sourceMappingURL=CliError.d.ts.map