/**
 * Global flags for Effect CLI command trees. Global flags are parsed outside a
 * single command's local flags and can apply to a command and its descendants.
 *
 * This module defines two kinds of global flags: action flags, which run an
 * effect and stop normal command execution, and setting flags, which provide a
 * parsed value to the command handler through the Effect context. It also
 * defines the built-in help, version, wizard, shell-completion, and log-level flags
 * used by `Command.run` and `Command.runWith`.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Console from "../Console.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as Option from "../Option.js";
import * as CliOutput from "./CliOutput.js";
import * as Completions_ from "./Completions.js";
import * as Flag from "./Flag.js";
import * as CommandDescriptor from "./internal/completions/descriptor.js";
import * as HelpInternal from "./internal/help.js";
/* ========================================================================== */
/* Constructors                                                               */
/* ========================================================================== */
/**
 * Creates an Action flag that performs a side effect and exits.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Action = options => ({
  _tag: "Action",
  flag: options.flag,
  run: options.run
});
/**
 * Creates a Setting flag that configures the command handler's environment.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Setting = id => options => {
  settingIdCounter += 1;
  const ref = Context.Service(`effect/cli/GlobalFlag/${id}/${settingIdCounter}`);
  return Object.assign(ref, {
    _tag: "Setting",
    id,
    flag: options.flag
  });
};
let settingIdCounter = 0;
/* ========================================================================== */
/* Built-in Flag References                                                   */
/* ========================================================================== */
/**
 * Defines the `--help` / `-h` global flag, which shows help documentation for the
 * active command path.
 *
 * @see {@link BuiltIns} for the default list containing this flag
 * @see {@link Action} for defining custom action global flags
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export const Help = /*#__PURE__*/Action({
  flag: /*#__PURE__*/Flag.Boolean("help").pipe(/*#__PURE__*/Flag.withAlias("h"), /*#__PURE__*/Flag.withDescription("Show help information"), /*#__PURE__*/Flag.withDefault(false)),
  run: /*#__PURE__*/Effect.fnUntraced(function* (_, {
    builtIns,
    command,
    commandPath
  }) {
    const formatter = yield* CliOutput.Formatter;
    const helpDoc = yield* HelpInternal.getHelpForCommandPath(command, commandPath, builtIns);
    yield* Console.log(formatter.formatHelpDoc(helpDoc));
  })
});
/**
 * Defines the global action flag for showing command version information.
 *
 * **When to use**
 *
 * Use to add a built-in `--version / -v` flag to a command runner.
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export const Version = /*#__PURE__*/Action({
  flag: /*#__PURE__*/Flag.Boolean("version").pipe(/*#__PURE__*/Flag.withAlias("v"), /*#__PURE__*/Flag.withDescription("Show version information"), /*#__PURE__*/Flag.withDefault(false)),
  run: /*#__PURE__*/Effect.fnUntraced(function* (_, {
    command,
    version
  }) {
    const formatter = yield* CliOutput.Formatter;
    yield* Console.log(formatter.formatVersion(command.name, version));
  })
});
/**
 * Defines the global action flag for starting interactive wizard mode.
 *
 * **Details**
 *
 * `Command.run` and `Command.runWith` handle this action specially so the
 * generated arguments can be passed back through the command parser.
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export const Wizard = /*#__PURE__*/Action({
  flag: /*#__PURE__*/Flag.Boolean("wizard").pipe(/*#__PURE__*/Flag.withDescription("Start wizard mode for a command"), /*#__PURE__*/Flag.withDefault(false)),
  run: () => Effect.void
});
/**
 * Defines the `--completions` global flag, which prints a shell completion script for
 * the given shell.
 *
 * **Details**
 *
 * Accepted values are `bash`, `zsh`, `fish`, and `sh`; `sh` is normalized to
 * `bash`.
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export const Completions = /*#__PURE__*/Action({
  flag: /*#__PURE__*/Flag.Literals("completions", ["bash", "zsh", "fish", "sh"]).pipe(Flag.optional, /*#__PURE__*/Flag.map(v => Option.map(v, s => s === "sh" ? "bash" : s)), /*#__PURE__*/Flag.withMetavar("<bash|zsh|fish|sh>"), /*#__PURE__*/Flag.withDescription("Print shell completion script")),
  run: /*#__PURE__*/Effect.fnUntraced(function* (shell, {
    command
  }) {
    if (Option.isNone(shell)) return;
    const descriptor = CommandDescriptor.fromCommand(command);
    yield* Console.log(Completions_.generate(command.name, shell.value, descriptor));
  })
});
/**
 * Defines the global setting flag for command log level.
 *
 * **When to use**
 *
 * Use to add a built-in `--log-level` option that configures the minimum log
 * level for the command.
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export const LogLevel = /*#__PURE__*/Setting("log-level")({
  flag: /*#__PURE__*/Flag.ChoiceWithValue("log-level", [["all", "All"], ["trace", "Trace"], ["debug", "Debug"], ["info", "Info"], ["warn", "Warn"], ["warning", "Warn"], ["error", "Error"], ["fatal", "Fatal"], ["none", "None"]]).pipe(Flag.optional, /*#__PURE__*/Flag.withDescription("Sets the minimum log level"), /*#__PURE__*/Flag.withMetavar("<all|trace|debug|info|warn|warning|error|fatal|none>"))
});
/* ========================================================================== */
/* References                                                                 */
/* ========================================================================== */
/**
 * Built-in global flags in default precedence order.
 *
 * **When to use**
 *
 * Use when extending or inspecting the default global-flag set that
 * `Command.runWith` prepends before user-defined global flags.
 *
 * **Details**
 *
 * The built-ins are `Help`, `Version`, `Wizard`, `Completions`, and `LogLevel`.
 * `Command.runWith` prepends these built-ins when collecting and parsing global
 * flags.
 *
 * **Gotchas**
 *
 * Action flags are processed in active flag order and the first present action
 * exits, so this array controls built-in action precedence.
 *
 * @see {@link Help} for the help action flag
 * @see {@link Version} for the version action flag
 * @see {@link Completions} for the shell-completions action flag
 * @see {@link LogLevel} for the built-in log-level setting flag
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export const BuiltIns = [Help, Version, Wizard, Completions, LogLevel];
//# sourceMappingURL=GlobalFlag.js.map