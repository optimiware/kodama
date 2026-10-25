import * as Console from "../Console.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import { dual } from "../Function.js";
import * as Option from "../Option.js";
import * as Predicate from "../Predicate.js";
import * as References from "../References.js";
import * as Result from "../Result.js";
import * as Runtime from "../Runtime.js";
import * as Stdio from "../Stdio.js";
import * as Terminal from "../Terminal.js";
import * as CliConfig from "./CliConfig.js";
import * as CliError from "./CliError.js";
import * as CliOutput from "./CliOutput.js";
import * as GlobalFlag from "./GlobalFlag.js";
import { checkForDuplicateFlags, makeCommand, makeParser, toImpl, TypeId } from "./internal/command.js";
import { mergeConfig, parseConfig } from "./internal/config.js";
import { getGlobalFlagsForCommandPath, getGlobalFlagsForCommandTree, getHelpForCommandPath } from "./internal/help.js";
import * as Lexer from "./internal/lexer.js";
import * as Parser from "./internal/parser.js";
import * as Wizard from "./internal/wizard.js";
import * as Param from "./Param.js";
import * as Prompt from "./Prompt.js";
/**
 * Returns `true` if the provided value is a `Command`.
 *
 * **Gotchas**
 *
 * This checks for the `Command` type-id property; it does not validate the full
 * command shape.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isCommand = u => Predicate.hasProperty(u, TypeId);
/* ========================================================================== */
/* Constructors                                                               */
/* ========================================================================== */
/**
 * Creates a `Command` from a name, an optional configuration, and an optional
 * handler.
 *
 * **Details**
 *
 * Use `withDescription` and related metadata combinators to add help text. The
 * overloads support simple commands, configured commands, and commands with
 * effectful handlers.
 *
 * **Example** (Creating commands)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Argument, Command, Flag } from "effect/cli"
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
 * // Simple command with no configuration
 * const version = Command.make("version")
 *
 * // Command with simple flags
 * const greet = Command.make("greet", {
 *   name: Flag.String("name"),
 *   count: Flag.Int("count").pipe(Flag.withDefault(1))
 * })
 *
 * // Command with nested configuration
 * const deploy = Command.make("deploy", {
 *   environment: Flag.String("env").pipe(
 *     Flag.withDescription("Target environment")
 *   ),
 *   server: {
 *     host: Flag.String("host").pipe(Flag.withDefault("localhost")),
 *     port: Flag.Int("port").pipe(Flag.withDefault(3000))
 *   },
 *   files: Argument.String("files").pipe(Argument.variadic),
 *   force: Flag.Boolean("force").pipe(
 *     Flag.withDescription("Force deployment"),
 *     Flag.withDefault(false)
 *   )
 * })
 *
 * // Command with handler
 * const output: Array<string> = []
 * const deployWithHandler = Command.make("deploy", {
 *   environment: Flag.String("env"),
 *   force: Flag.Boolean("force").pipe(Flag.withDefault(false))
 * }, (config) =>
 *   Effect.gen(function*() {
 *     yield* Effect.sync(() => output.push(`Starting deployment to ${config.environment}`))
 *
 *     if (!config.force && config.environment === "production") {
 *       return yield* Effect.fail("Production deployments require --force flag")
 *     }
 *
 *     yield* Effect.sync(() => output.push("Deployment completed successfully"))
 *   }))
 *
 * await Effect.runPromise(
 *   Command.runWith(deployWithHandler, { version: "1.0.0" })([
 *     "--env",
 *     "staging",
 *     "--force"
 *   ]).pipe(Effect.provide(CliTestLayer))
 * )
 * output // => ["Starting deployment to staging", "Deployment completed successfully"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = (name, config, handler) => {
  const parsedConfig = parseConfig(config ?? {});
  return makeCommand({
    name,
    config: parsedConfig,
    ...(Predicate.isNotUndefined(handler) ? {
      handle: handler
    } : {})
  });
};
/* ========================================================================== */
/* Combinators                                                                */
/* ========================================================================== */
/**
 * Adds or replaces the handler for a command.
 *
 * **Example** (Adding command handlers)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Command, Flag } from "effect/cli"
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
 * // Command without initial handler
 * const greet = Command.make("greet", {
 *   name: Flag.String("name")
 * })
 *
 * // Add handler later
 * const output: Array<string> = []
 * const greetWithHandler = greet.pipe(
 *   Command.withHandler((config: { readonly name: string }) =>
 *     Effect.sync(() => output.push(`Hello, ${config.name}!`)).pipe(Effect.asVoid)
 *   )
 * )
 *
 * await Effect.runPromise(
 *   Command.runWith(greetWithHandler, { version: "1.0.0" })(["--name", "Alice"]).pipe(
 *     Effect.provide(CliTestLayer)
 *   )
 * )
 * output // => ["Hello, Alice!"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withHandler = /*#__PURE__*/dual(2, (self, handler) => makeCommand({
  ...toImpl(self),
  handle: handler
}));
const normalizeSubcommandEntries = entries => {
  const flat = [];
  const grouped = new Map();
  const addToGroup = (group, command) => {
    flat.push(command);
    const existing = grouped.get(group);
    if (existing) {
      existing.push(command);
    } else {
      grouped.set(group, [command]);
    }
  };
  for (const entry of entries) {
    if (isCommand(entry)) {
      addToGroup(undefined, entry);
      continue;
    }
    for (const command of entry.commands) {
      addToGroup(entry.group, command);
    }
  }
  const groups = [];
  const ungroupedCommands = grouped.get(undefined);
  if (ungroupedCommands && ungroupedCommands.length > 0) {
    groups.push({
      group: undefined,
      commands: ungroupedCommands
    });
  }
  for (const [group, commands] of grouped) {
    if (group === undefined) {
      continue;
    }
    groups.push({
      group,
      commands
    });
  }
  return {
    flat,
    groups
  };
};
/**
 * Adds subcommands to a command, creating a hierarchical command structure.
 *
 * **Details**
 *
 * Subcommands can access their parent's parsed configuration by yielding the parent
 * command within their handler. This enables shared parent flags that affect
 * all subcommands.
 *
 * **Example** (Adding subcommands)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Command, Flag } from "effect/cli"
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
 * // Parent command with shared flags
 * const git = Command.make("git").pipe(
 *   Command.withSharedFlags({
 *     verbose: Flag.Boolean("verbose").pipe(Flag.withDefault(false))
 *   })
 * )
 *
 * // Subcommand that accesses parent config
 * const output: Array<string> = []
 * const clone = Command.make("clone", {
 *   repository: Flag.String("repo")
 * }, (config) =>
 *   Effect.gen(function*() {
 *     const parent = yield* git // Access parent's parsed config
 *     if (parent.verbose) {
 *       yield* Effect.sync(() => output.push("Verbose mode enabled"))
 *     }
 *     yield* Effect.sync(() => output.push(`Cloning ${config.repository}`))
 *   }))
 *
 * const app = git.pipe(Command.withSubcommands([clone]))
 *
 * await Effect.runPromise(
 *   Command.runWith(app, { version: "1.0.0" })([
 *     "--verbose",
 *     "clone",
 *     "--repo",
 *     "github.com/foo/bar"
 *   ]).pipe(Effect.provide(CliTestLayer))
 * )
 * output // => ["Verbose mode enabled", "Cloning github.com/foo/bar"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withSubcommands = /*#__PURE__*/dual(2, (self, subcommands) => {
  const normalized = normalizeSubcommandEntries(subcommands);
  checkForDuplicateFlags(self, normalized.flat);
  const impl = toImpl(self);
  const byName = new Map(normalized.flat.map(s => [s.name, toImpl(s)]));
  const SubcommandStateSymbol = Symbol("effect/cli/SubcommandState");
  const parse = Effect.fnUntraced(function* (raw) {
    if (Option.isNone(raw.subcommand)) {
      return yield* impl.parse(raw);
    }
    const sub = byName.get(raw.subcommand.value.name);
    if (!sub) {
      return yield* impl.parse(raw);
    }
    const context = yield* impl.parseContext(raw);
    const result = yield* sub.parse(raw.subcommand.value.parsedInput);
    return {
      ...context,
      [SubcommandStateSymbol]: {
        name: sub.name,
        result
      }
    };
  });
  const handle = Effect.fnUntraced(function* (input, path) {
    const internal = input;
    const selectedSubcommand = internal[SubcommandStateSymbol];
    if (selectedSubcommand) {
      const child = byName.get(selectedSubcommand.name);
      if (!child) {
        return yield* new CliError.ShowHelp({
          commandPath: path,
          errors: []
        });
      }
      return yield* child.handle(selectedSubcommand.result, [...path, child.name]).pipe(Effect.provideService(impl.service, input));
    }
    return yield* impl.handle(input, path);
  });
  return makeCommand({
    name: impl.name,
    config: impl.config,
    contextConfig: impl.contextConfig,
    description: impl.description,
    shortDescription: impl.shortDescription,
    alias: impl.alias,
    unlisted: impl.unlisted,
    annotations: impl.annotations,
    globalFlags: impl.globalFlags,
    examples: impl.examples,
    service: impl.service,
    subcommands: normalized.groups,
    parse,
    parseContext: impl.parseContext,
    handle
  });
});
/**
 * Adds flags that are inherited by subcommands.
 *
 * **Details**
 *
 * Shared flags are available to this command's handler and to descendant
 * handlers via `yield* parentCommand`. Shared flags are accepted both before
 * and after a selected subcommand name (npm-style).
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withSharedFlags = /*#__PURE__*/dual(2, (self, sharedFlags) => {
  const impl = toImpl(self);
  const sharedConfig = parseConfig(sharedFlags);
  const mergedConfig = mergeConfig(impl.config, sharedConfig);
  const mergedContextConfig = mergeConfig(impl.contextConfig, sharedConfig);
  if (impl.subcommands.length > 0) {
    const flatSubcommands = impl.subcommands.flatMap(group => group.commands);
    checkForDuplicateFlags(self, flatSubcommands, {
      contextConfig: mergedContextConfig
    });
  }
  const parseShared = makeParser(sharedConfig, {
    allowLeftovers: true
  });
  const parse = Effect.fnUntraced(function* (raw) {
    const base = yield* impl.parse(raw);
    const shared = yield* parseShared(raw);
    return {
      ...base,
      ...shared
    };
  });
  const parseContext = Effect.fnUntraced(function* (raw) {
    const base = yield* impl.parseContext(raw);
    const shared = yield* parseShared(raw);
    return {
      ...base,
      ...shared
    };
  });
  const handle = (input, commandPath) => impl.handle(input, commandPath);
  return makeCommand({
    name: impl.name,
    config: mergedConfig,
    contextConfig: mergedContextConfig,
    description: impl.description,
    shortDescription: impl.shortDescription,
    alias: impl.alias,
    unlisted: impl.unlisted,
    annotations: impl.annotations,
    globalFlags: impl.globalFlags,
    examples: impl.examples,
    service: impl.service,
    subcommands: impl.subcommands,
    parse,
    parseContext,
    handle
  });
});
/**
 * Adds global flags to a command scope.
 *
 * **Details**
 *
 * Declared global flags apply to the command and all of its descendants.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withGlobalFlags = /*#__PURE__*/dual(2, (self, globalFlags) => {
  const impl = toImpl(self);
  const next = Array.from(new Set([...impl.globalFlags, ...globalFlags]));
  return makeCommand({
    ...impl,
    globalFlags: next
  });
});
/**
 * Sets the description for a command.
 *
 * **Details**
 *
 * Descriptions provide users with information about what the command does
 * when they view help documentation.
 *
 * **Example** (Setting descriptions)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Command, Flag } from "effect/cli"
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
 * const output: Array<string> = []
 * const deploy = Command.make("deploy", {
 *   environment: Flag.String("env")
 * }, (config) =>
 *   Effect.gen(function*() {
 *     yield* Effect.sync(() => output.push(`Deploying to ${config.environment}`))
 *   })).pipe(
 *     Command.withDescription("Deploy the application to a specified environment")
 *   )
 *
 * await Effect.runPromise(
 *   Command.runWith(deploy, { version: "1.0.0" })(["--env", "staging"]).pipe(Effect.provide(CliTestLayer))
 * )
 * output // => ["Deploying to staging"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withDescription = /*#__PURE__*/dual(2, (self, description) => makeCommand({
  ...toImpl(self),
  description
}));
/**
 * Sets a short description for a command.
 *
 * **Details**
 *
 * Short descriptions are used when listing subcommands in help output and
 * shell completions. If no short description is provided, the full
 * `description` is used as a fallback.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withShortDescription = /*#__PURE__*/dual(2, (self, shortDescription) => makeCommand({
  ...toImpl(self),
  shortDescription
}));
/**
 * Sets an alias for a command.
 *
 * **Details**
 *
 * Aliases are accepted as alternate subcommand names during parsing and are
 * shown in help output as `name, alias`.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withAlias = /*#__PURE__*/dual(2, (self, alias) => makeCommand({
  ...toImpl(self),
  alias
}));
/**
 * Omits a subcommand from parent help output, shell completions, and
 * "did you mean?" suggestions while keeping it fully invocable by exact name.
 *
 * **When to use**
 *
 * Use when experimental or internal subcommands should be accepted but not advertised on
 * the public CLI surface.
 *
 * **Example** (Unlisting a subcommand)
 *
 * ```ts import.meta.vitest
 * import { Command } from "effect/cli"
 *
 * // `experimental` still runs when invoked as `mycli experimental`,
 * // but it does not appear under SUBCOMMANDS in `mycli --help`.
 * const experimental = Command.make("experimental").pipe(
 *   Command.unlisted
 * )
 *
 * const root = Command.make("mycli").pipe(
 *   Command.withSubcommands([experimental])
 * )
 *
 * root.subcommands[0].commands[0].unlisted // => true
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const unlisted = self => makeCommand({
  ...toImpl(self),
  unlisted: true
});
/**
 * Adds a custom annotation to a command.
 *
 * **When to use**
 *
 * Use to attach one command-scoped metadata value under a `Context.Key`,
 * especially for consumers such as custom help formatters.
 *
 * **Details**
 *
 * Annotations are stored on the command's annotation context and flow into
 * generated help document annotations.
 *
 * **Gotchas**
 *
 * Adding the same `Context.Key` again replaces the earlier value.
 *
 * @see {@link annotateMerge} for merging an existing annotation context
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const annotate = /*#__PURE__*/dual(3, (self, service, value) => {
  const impl = toImpl(self);
  return makeCommand({
    ...impl,
    annotations: Context.add(impl.annotations, service, value)
  });
});
/**
 * Merges a Context of annotations into a command.
 *
 * **When to use**
 *
 * Use when you need to attach an already-built `Context.Context` of command
 * annotations.
 *
 * **Details**
 *
 * Merged annotations are stored on the command and exposed through generated
 * help document annotations.
 *
 * **Gotchas**
 *
 * If both contexts contain the same `Context.Key`, the incoming annotations
 * context wins.
 *
 * @see {@link annotate} for adding a single annotation without constructing a `Context`
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const annotateMerge = /*#__PURE__*/dual(2, (self, annotations) => {
  const impl = toImpl(self);
  return makeCommand({
    ...impl,
    annotations: Context.merge(impl.annotations, annotations)
  });
});
/**
 * Sets usage examples for a command.
 *
 * **Details**
 *
 * Examples are exposed in structured `HelpDoc` data and rendered by the
 * default formatter in an `EXAMPLES` section.
 *
 * **Example** (Adding usage examples)
 *
 * ```ts import.meta.vitest
 * import { Command } from "effect/cli"
 *
 * const login = Command.make("login").pipe(
 *   Command.withExamples([
 *     { command: "myapp login", description: "Log in with browser OAuth" },
 *     { command: "myapp login --token sbp_abc123", description: "Log in with a token" }
 *   ])
 * )
 *
 * login.examples.map((example) => example.command) // => ["myapp login", "myapp login --token sbp_abc123"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const withExamples = /*#__PURE__*/dual(2, (self, examples) => makeCommand({
  ...toImpl(self),
  examples
}));
/* ========================================================================== */
/* Providing Services                                                         */
/* ========================================================================== */
// Internal helper: transforms a command's handler while preserving other properties
const mapHandler = (self, f) => {
  const impl = toImpl(self);
  return makeCommand({
    ...impl,
    handle: (input, path) => f(impl.handle(input, path), input)
  });
};
/**
 * Provides the handler of a command with the services produced by a layer
 * that optionally depends on the command-line input to be created.
 *
 * **Example** (Providing command services)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, PlatformError, Stdio, Terminal } from "effect"
 * import { Command, Flag } from "effect/cli"
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
 * const output: Array<string> = []
 * const deploy = Command.make("deploy", {
 *   env: Flag.String("env")
 * }, (config) =>
 *   Effect.gen(function*() {
 *     const fs = yield* FileSystem.FileSystem
 *     yield* Effect.sync(() => output.push(`Using file system for ${config.env}`))
 *   })).pipe(
 *     // Provide FileSystem based on the --env flag
 *     Command.provide((config) =>
 *       config.env === "local"
 *         ? FileSystem.layerNoop({})
 *         : FileSystem.layerNoop({
 *           access: () =>
 *             Effect.fail(
 *               PlatformError.badArgument({
 *                 module: "FileSystem",
 *                 method: "access"
 *               })
 *             )
 *         })
 *     )
 *   )
 *
 * await Effect.runPromise(
 *   Command.runWith(deploy, { version: "1.0.0" })(["--env", "local"]).pipe(Effect.provide(CliTestLayer))
 * )
 * output // => ["Using file system for local"]
 * ```
 *
 * @stability unstable
 * @category providing services
 * @since 4.0.0
 */
export const provide = /*#__PURE__*/dual(args => isCommand(args[0]), (self, layer, options) => mapHandler(self, (handler, input) => Effect.provide(handler, typeof layer === "function" ? layer(input) : layer, options)));
/**
 * Provides the handler of a command with the implementation of a service that
 * optionally depends on the command-line input to be constructed.
 *
 * **When to use**
 *
 * Use when a command handler needs a pure service implementation, optionally
 * derived from the parsed command input.
 *
 * @stability unstable
 * @category providing services
 * @since 4.0.0
 */
export const provideSync = /*#__PURE__*/dual(3, (self, service, implementation) => mapHandler(self, (handler, input) => Effect.provideService(handler, service, typeof implementation === "function" ? implementation(input) : implementation)));
/**
 * Provides the handler of a command with the service produced by an effect
 * that optionally depends on the command-line input to be created.
 *
 * **When to use**
 *
 * Use to acquire a service effectfully for each command run, optionally using
 * parsed command input.
 *
 * @see {@link provideSync} for synchronous service acquisition
 * @see {@link provide} for providing an already-available service
 * @see {@link provideEffectDiscard} for running an effect before the handler without providing a service
 *
 * @stability unstable
 * @category providing services
 * @since 4.0.0
 */
export const provideEffect = /*#__PURE__*/dual(3, (self, service, effect) => mapHandler(self, (handler, input) => Effect.provideServiceEffect(handler, service, typeof effect === "function" ? effect(input) : effect)));
/**
 * Allows for execution of an effect, which optionally depends on command-line
 * input to be created, prior to executing the handler of a command.
 *
 * @stability unstable
 * @category providing services
 * @since 4.0.0
 */
export const provideEffectDiscard = /*#__PURE__*/dual(2, (self, effect) => mapHandler(self, (handler, input) => Effect.andThen(typeof effect === "function" ? effect(input) : effect, handler)));
/* ========================================================================== */
/* Execution                                                                  */
/* ========================================================================== */
/**
 * Interactively constructs command-line arguments for a command.
 *
 * **Details**
 *
 * The returned arguments include the command name and can be inspected,
 * modified, or passed to another command runner by the caller.
 *
 * **Example** (Constructing command arguments)
 *
 * ```ts import.meta.vitest
 * import { Console, Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Command } from "effect/cli"
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
 * const command = Command.make("app")
 * const silentConsole: Console.Console = Object.assign(Object.create(console), {
 *   log: () => {}
 * })
 *
 * const program = Command.wizard(command).pipe(
 *   Effect.provideService(Console.Console, silentConsole),
 *   Effect.provide(CliTestLayer)
 * )
 *
 * await Effect.runPromise(program) // => ["app"]
 * ```
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export const wizard = (command, options) => Effect.map(Wizard.run(command, options), result => result.args);
const getOutOfScopeGlobalFlagErrors = (allFlags, activeFlags, flagMap, commandPath) => {
  const activeSet = new Set(activeFlags);
  const errors = [];
  const seen = new Set();
  for (const flag of allFlags) {
    if (activeSet.has(flag)) {
      continue;
    }
    const singles = Param.extractSingleParams(flag.flag);
    for (const single of singles) {
      const entries = flagMap[single.name];
      if (!entries || entries.length === 0) {
        continue;
      }
      const option = `--${single.name}`;
      if (seen.has(option)) {
        continue;
      }
      seen.add(option);
      errors.push(new CliError.UnrecognizedOption({
        option,
        suggestions: [],
        command: commandPath
      }));
    }
  }
  return errors;
};
const showHelp = (command, error, renderErrors) => Effect.gen(function* () {
  const {
    builtIns
  } = yield* CliConfig.CliConfig;
  const formatter = yield* CliOutput.Formatter;
  const helpDoc = yield* getHelpForCommandPath(command, error.commandPath, builtIns);
  yield* Console.log(formatter.formatHelpDoc(helpDoc));
  if (renderErrors && error.errors.length > 0) {
    yield* Console.error(formatter.formatErrors(error.errors));
  }
});
const showUserError = error => Effect.gen(function* () {
  const formatter = yield* CliOutput.Formatter;
  yield* Console.error(formatter.formatError(error));
  error[Runtime.errorReported] = false;
});
/**
 * Runs a command using the arguments supplied by the `Stdio` service.
 *
 * **When to use**
 *
 * Use when command-line arguments should come from `Stdio` at the application
 * entry point.
 *
 * Help documents are always rendered. By default, parse error details and
 * `CliError.UserError` failures are also rendered with the installed
 * `CliOutput.Formatter` before the error is rethrown. Set `renderErrors` to
 * `false` when the host application owns error rendering.
 *
 * **Example** (Running commands with standard input)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Command, Flag } from "effect/cli"
 * import { ChildProcessSpawner } from "effect/process"
 *
 * const CliTestLayer = Layer.mergeAll(
 *   FileSystem.layerNoop({}),
 *   Path.layer,
 *   Stdio.layerTest({
 *     args: Effect.succeed(["--name", "Alice"])
 *   }),
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
 * const output: Array<string> = []
 * const greetCommand = Command.make("greet", {
 *   name: Flag.String("name")
 * }, (config) =>
 *   Effect.gen(function*() {
 *     yield* Effect.sync(() => output.push(`Hello, ${config.name}!`))
 *   }))
 *
 * // Automatically gets args from the Stdio service
 * const program = Command.run(greetCommand, {
 *   version: "1.0.0"
 * })
 *
 * await Effect.runPromise(program.pipe(Effect.provide(CliTestLayer)))
 * output // => ["Hello, Alice!"]
 * ```
 *
 * @see {@link runWith} for running a command with an explicit argument array
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export const run = /*#__PURE__*/dual(2, (command, config) => Stdio.Stdio.use(({
  args
}) => Effect.flatMap(args, args => runWith(command, config)(args))));
/**
 * Runs a command with explicitly provided arguments instead of using arguments from `Stdio`.
 *
 * **When to use**
 *
 * Use when you need to test CLI applications or programmatically execute
 * commands with specific arguments.
 *
 * Help documents are always rendered. By default, parse error details and
 * `CliError.UserError` failures are also rendered with the installed
 * `CliOutput.Formatter` before the error is rethrown. Set `renderErrors` to
 * `false` when the host application owns error rendering.
 *
 * **Example** (Running commands with explicit arguments)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Stdio, Terminal } from "effect"
 * import { Command, Flag } from "effect/cli"
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
 * const output: Array<string> = []
 * const greet = Command.make("greet", {
 *   name: Flag.String("name"),
 *   count: Flag.Int("count").pipe(Flag.withDefault(1))
 * }, (config) =>
 *   Effect.gen(function*() {
 *     for (let i = 0; i < config.count; i++) {
 *       yield* Effect.sync(() => output.push(`Hello, ${config.name}!`))
 *     }
 *   }))
 *
 * // Test with specific arguments
 * const testProgram = Effect.gen(function*() {
 *   const runCommand = Command.runWith(greet, { version: "1.0.0" })
 *
 *   yield* runCommand(["--name", "Alice", "--count", "2"])
 * })
 *
 * await Effect.runPromise(testProgram.pipe(Effect.provide(CliTestLayer)))
 * output // => ["Hello, Alice!", "Hello, Alice!"]
 * ```
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export const runWith = (command, config) => {
  const commandImpl = toImpl(command);
  return Effect.fnUntraced(function* (args) {
    const {
      builtIns
    } = yield* CliConfig.CliConfig;
    const {
      tokens,
      trailingOperands
    } = Lexer.lex(args);
    // 1. Collect known global flags from the command tree
    const allFlags = getGlobalFlagsForCommandTree(command, builtIns);
    // 2. Extract global flag tokens
    const allFlagParams = allFlags.flatMap(f => Param.extractSingleParams(f.flag));
    const globalRegistry = Parser.createFlagRegistry(allFlagParams.filter(Param.isFlagParam));
    const {
      flagMap,
      remainder,
      errors: globalFlagErrors
    } = Parser.consumeGlobalFlags(tokens, command, globalRegistry);
    const emptyArgs = {
      flags: flagMap,
      arguments: []
    };
    // 3. Parse command arguments from remaining tokens
    const parsedArgs = yield* Parser.parseArgs({
      tokens: remainder,
      trailingOperands
    }, command);
    const commandPath = [command.name, ...Parser.getCommandPath(parsedArgs)];
    const handlerCtx = {
      builtIns,
      command,
      commandPath,
      version: config.version
    };
    const activeFlags = getGlobalFlagsForCommandPath(command, commandPath, builtIns);
    // 4. Reject globals that were passed outside the active command scope
    const outOfScopeErrors = getOutOfScopeGlobalFlagErrors(allFlags, activeFlags, flagMap, commandPath);
    if (outOfScopeErrors.length > 0 || globalFlagErrors.length > 0) {
      const parseErrors = parsedArgs.errors ?? [];
      return yield* new CliError.ShowHelp({
        commandPath,
        errors: [...globalFlagErrors, ...outOfScopeErrors, ...parseErrors]
      });
    }
    // 5. Process action flags — first present action wins, then exit
    for (const flag of activeFlags) {
      if (flag._tag !== "Action") continue;
      const singles = Param.extractSingleParams(flag.flag);
      const hasEntry = singles.some(s => {
        const entries = flagMap[s.name];
        return entries !== undefined && entries.length > 0;
      });
      if (!hasEntry) continue;
      const [, value] = yield* flag.flag.parse(emptyArgs);
      if (flag === GlobalFlag.Wizard) {
        return yield* Effect.gen(function* () {
          yield* Console.log(Wizard.renderIntroduction(command.name, config.version, command.description));
          const prefix = [command.name, ...args.filter(arg => arg !== "--wizard" && !arg.startsWith("--wizard="))];
          const wizardResult = yield* Wizard.run(command, {
            commandPath,
            prefix
          });
          yield* Console.log(Wizard.renderCompletion(wizardResult.displayArgs));
          const shouldRun = yield* Prompt.run(Prompt.Toggle({
            message: "Run this command?",
            initial: true,
            active: "yes",
            inactive: "no"
          }));
          if (shouldRun) {
            yield* Console.log();
            yield* runWith(command, {
              ...config,
              renderErrors: false
            })(wizardResult.args.slice(1));
          }
        }).pipe(Effect.catchTag("QuitError", () => Console.log(Wizard.renderQuit())));
      }
      yield* flag.run(value, handlerCtx);
      return;
    }
    // 6. Handle parsing errors
    if (parsedArgs.errors && parsedArgs.errors.length > 0) {
      return yield* new CliError.ShowHelp({
        commandPath,
        errors: parsedArgs.errors
      });
    }
    const parseResult = yield* Effect.result(commandImpl.parse(parsedArgs));
    if (parseResult._tag === "Failure") {
      return yield* new CliError.ShowHelp({
        commandPath,
        errors: [parseResult.failure]
      });
    }
    // 7. Provide setting values
    let program = commandImpl.handle(parseResult.success, [command.name]);
    const logLevel = activeFlags.includes(GlobalFlag.LogLevel) ? (yield* GlobalFlag.LogLevel.flag.parse(emptyArgs))[1] : Option.none();
    program = Effect.provideService(program, GlobalFlag.LogLevel, logLevel);
    for (const flag of activeFlags) {
      if (flag._tag !== "Setting" || flag === GlobalFlag.LogLevel) continue;
      const [, value] = yield* flag.flag.parse(emptyArgs);
      program = Effect.provideService(program, flag, value);
    }
    // 8. Apply built-in setting behavior
    const services = Option.match(logLevel, {
      onNone: () => Context.empty(),
      onSome: level => Context.make(References.MinimumLogLevel, level)
    });
    // 9. Run command handler with composed context
    yield* Effect.provideContext(program, services);
  }, Effect.catchFilter(error => CliError.isCliError(error) && error._tag === "ShowHelp" ? Result.succeed(error) : Result.fail(error), error => Effect.andThen(showHelp(command, error, config.renderErrors !== false), Effect.fail(error))), Effect.catchFilter(error => config.renderErrors !== false && CliError.isCliError(error) && error._tag === "UserError" ? Result.succeed(error) : Result.fail(error), error => Effect.andThen(showUserError(error), Effect.fail(error))), Effect.catchFilter(e => Terminal.isQuitError(e) ? Result.succeed(e) : Result.fail(e), _ => Effect.interrupt));
};
//# sourceMappingURL=Command.js.map