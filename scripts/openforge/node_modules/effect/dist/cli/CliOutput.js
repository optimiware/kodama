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
import * as Context from "../Context.js";
import * as Layer from "../Layer.js";
import * as Option from "../Option.js";
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
export const Formatter = /*#__PURE__*/Context.Reference("effect/cli/CliOutput", {
  defaultValue: () => defaultFormatter()
});
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
export const layer = formatter => Layer.succeed(Formatter)(formatter);
const escapeControlCharacters = text =>
// oxlint-disable-next-line no-control-regex
text.replace(/[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/g, character => `\\x${character.charCodeAt(0).toString(16).padStart(2, "0")}`);
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
export const defaultFormatter = options => {
  const globalProcess = globalThis.process;
  const hasProcess = typeof globalProcess === "object" && globalProcess !== null;
  const useColor = options?.colors !== undefined ? options.colors
  // Auto-detect based on environment
  : hasProcess && typeof globalProcess.stdout === "object" && globalProcess.stdout !== null && globalProcess.stdout.isTTY === true && !globalProcess.env?.NO_COLOR;
  // Color palette using ANSI escape codes
  const colors = useColor ? {
    bold: text => `\x1b[1m${text}\x1b[0m`,
    dim: text => `\x1b[2m${text}\x1b[0m`,
    cyan: text => `\x1b[36m${text}\x1b[0m`,
    green: text => `\x1b[32m${text}\x1b[0m`,
    blue: text => `\x1b[34m${text}\x1b[0m`,
    yellow: text => `\x1b[33m${text}\x1b[0m`,
    magenta: text => `\x1b[35m${text}\x1b[0m`
  } : {
    bold: text => text,
    dim: text => text,
    cyan: text => text,
    green: text => text,
    blue: text => text,
    yellow: text => text,
    magenta: text => text
  };
  const reset = useColor ? "\x1b[0m" : "";
  const red = useColor ? "\x1b[31m" : "";
  const bold = useColor ? "\x1b[1m" : "";
  return {
    formatHelpDoc: doc => formatHelpDocImpl(doc, colors),
    formatCliError: error => escapeControlCharacters(error.message),
    formatError: error => {
      return `\n${bold}${red}ERROR${reset}\n  ${escapeControlCharacters(error.message)}${reset}`;
    },
    formatErrors: errors => {
      if (errors.length === 0) return "";
      if (errors.length === 1) {
        return `\n${bold}${red}ERROR${reset}\n  ${escapeControlCharacters(errors[0].message)}${reset}`;
      }
      // Group errors by _tag
      const grouped = new Map();
      for (const error of errors) {
        const tag = error._tag ?? "Error";
        const group = grouped.get(tag) ?? [];
        group.push(error);
        grouped.set(tag, group);
      }
      const sections = [];
      sections.push(`\n${bold}${red}ERRORS${reset}`);
      for (const [, group] of grouped) {
        for (const error of group) {
          sections.push(`  ${escapeControlCharacters(error.message)}${reset}`);
        }
      }
      return sections.join("\n");
    },
    formatVersion: (name, version) => `${colors.bold(name)} ${colors.dim("v")}${colors.bold(version)}`
  };
};
/**
 * Strips ANSI escape codes from a string to calculate visual width.
 * @internal
 */
const stripAnsi = text => {
  // oxlint-disable-next-line no-control-regex
  return text.replace(/\u001B\[[0-9;]*m/g, "");
};
/**
 * Gets the terminal display width of a string (excluding ANSI codes).
 * @internal
 */
const graphemeSegmenter = /*#__PURE__*/new Intl.Segmenter(undefined, {
  granularity: "grapheme"
});
const zeroWidthCodePoints = /[\p{Mark}\p{Control}\p{Default_Ignorable_Code_Point}]/gu;
const emojiPresentation = /\p{Emoji_Presentation}/u;
const isFullWidthCodePoint = codePoint => {
  if (codePoint < 0x1100) return false;
  return codePoint <= 0x115f || codePoint === 0x2329 || codePoint === 0x232a || codePoint >= 0x2e80 && codePoint <= 0x303e || codePoint >= 0x3040 && codePoint <= 0xa4cf || codePoint >= 0xac00 && codePoint <= 0xd7a3 || codePoint >= 0xf900 && codePoint <= 0xfaff || codePoint >= 0xfe10 && codePoint <= 0xfe19 || codePoint >= 0xfe30 && codePoint <= 0xfe6f || codePoint >= 0xff00 && codePoint <= 0xff60 || codePoint >= 0xffe0 && codePoint <= 0xffe6 || codePoint >= 0x1b000 && codePoint <= 0x1b2ff || codePoint >= 0x1f200 && codePoint <= 0x1f251 || codePoint >= 0x20000 && codePoint <= 0x3fffd;
};
const graphemeWidth = grapheme => {
  const visible = grapheme.replace(zeroWidthCodePoints, "");
  if (visible.length === 0) return 0;
  if (emojiPresentation.test(grapheme) || grapheme.includes("\uFE0F")) return 2;
  return isFullWidthCodePoint(visible.codePointAt(0)) ? 2 : 1;
};
const visualLength = text => {
  let length = 0;
  for (const {
    segment
  } of graphemeSegmenter.segment(stripAnsi(text))) {
    length += graphemeWidth(segment);
  }
  return length;
};
/**
 * Helper function to pad strings to a specified width.
 * @internal
 */
const pad = (s, width) => {
  const actualLength = visualLength(s);
  const padding = Math.max(0, width - actualLength);
  return s + " ".repeat(padding);
};
/**
 * Renders a table with aligned columns.
 * @internal
 */
const renderTable = (rows, widthCap) => {
  const maxColumn = Math.max(...rows.map(r => visualLength(r.left))) + 4;
  const col = widthCap === undefined ? maxColumn : Math.min(maxColumn, widthCap);
  return rows.map(({
    left,
    right
  }) => `  ${pad(left, Math.max(col, visualLength(left) + 1))}${right}`).join("\n");
};
const formatSubcommandName = (name, alias) => alias ? `${name}, ${alias}` : name;
/**
 * Internal implementation of help formatting that accepts configurable color functions.
 * @internal
 */
const formatHelpDocImpl = (doc, colors) => {
  const sections = [];
  // Description section
  if (doc.description) {
    sections.push(colors.bold("DESCRIPTION"));
    sections.push(`  ${doc.description}`);
    sections.push("");
  }
  // Usage section
  sections.push(colors.bold("USAGE"));
  sections.push(`  ${colors.cyan(doc.usage)}`);
  sections.push("");
  // Arguments section
  if (doc.args && doc.args.length > 0) {
    sections.push(colors.bold("ARGUMENTS"));
    const argRows = doc.args.map(arg => {
      let name = arg.name;
      if (arg.variadic) {
        name += "...";
      }
      const coloredName = colors.green(name);
      const coloredType = colors.dim(arg.type);
      const nameType = `${coloredName} ${coloredType}`;
      const optionalSuffix = arg.required ? "" : colors.dim(" (optional)");
      const description = Option.getOrElse(arg.description, () => "") + optionalSuffix;
      return {
        left: nameType,
        right: description
      };
    });
    sections.push(renderTable(argRows, 25));
    sections.push("");
  }
  // Flags section
  if (doc.flags.length > 0) {
    sections.push(colors.bold("FLAGS"));
    const flagRows = doc.flags.map(flag => {
      const names = [];
      // Add main name with -- prefix first
      names.push(colors.green(`--${flag.name}`));
      // Add aliases after (like -f) to match expected ordering
      for (const alias of flag.aliases) {
        names.push(colors.green(alias));
      }
      const namesPart = names.join(", ");
      const typePart = flag.type !== "boolean" ? ` ${colors.dim(flag.type)}` : "";
      return {
        left: namesPart + typePart,
        right: Option.getOrElse(flag.description, () => "")
      };
    });
    sections.push(renderTable(flagRows));
    sections.push("");
  }
  // Global Flags section
  if (doc.globalFlags && doc.globalFlags.length > 0) {
    sections.push(colors.bold("GLOBAL FLAGS"));
    const globalFlagRows = doc.globalFlags.map(flag => {
      const names = [];
      // Add main name with -- prefix first
      names.push(colors.green(`--${flag.name}`));
      // Add aliases after (like -f) to match expected ordering
      for (const alias of flag.aliases) {
        names.push(colors.green(alias));
      }
      const namesPart = names.join(", ");
      const typePart = flag.type !== "boolean" ? ` ${colors.dim(flag.type)}` : "";
      return {
        left: namesPart + typePart,
        right: Option.getOrElse(flag.description, () => "")
      };
    });
    sections.push(renderTable(globalFlagRows));
    sections.push("");
  }
  // Subcommands section
  if (doc.subcommands && doc.subcommands.length > 0) {
    const ungrouped = doc.subcommands.find(group => group.group === undefined);
    if (ungrouped) {
      sections.push(colors.bold("SUBCOMMANDS"));
      sections.push(renderTable(ungrouped.commands.map(sub => ({
        left: colors.cyan(formatSubcommandName(sub.name, sub.alias)),
        right: sub.shortDescription ?? sub.description
      })), 20));
      if (doc.subcommands.length > 1) {
        sections.push("");
      }
    }
    for (const group of doc.subcommands) {
      if (group.group === undefined) continue;
      sections.push(colors.bold(`${group.group}:`));
      sections.push(renderTable(group.commands.map(sub => ({
        left: colors.cyan(formatSubcommandName(sub.name, sub.alias)),
        right: sub.shortDescription ?? sub.description
      })), 20));
      sections.push("");
    }
  }
  // Examples section
  if (doc.examples && doc.examples.length > 0) {
    sections.push(colors.bold("EXAMPLES"));
    let first = true;
    let previousHadDescription = false;
    for (const example of doc.examples) {
      if (example.description) {
        if (!first) sections.push("");
        sections.push(`  ${colors.dim(`# ${example.description}`)}`);
      } else if (previousHadDescription) {
        sections.push("");
      }
      sections.push(`  ${colors.cyan(example.command)}`);
      first = false;
      previousHadDescription = !!example.description;
    }
    sections.push("");
  }
  // Remove trailing empty line if present
  if (sections[sections.length - 1] === "") {
    sections.pop();
  }
  return sections.join("\n");
};
//# sourceMappingURL=CliOutput.js.map