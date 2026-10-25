import * as Console from "../../Console.js";
import * as Effect from "../../Effect.js";
import * as Option from "../../Option.js";
import * as Redacted from "../../Redacted.js";
import * as Param from "../Param.js";
import * as Primitive from "../Primitive.js";
import * as Prompt from "../Prompt.js";
import * as Ansi from "./ansi.js";
import { toImpl } from "./command.js";
export const run = /*#__PURE__*/Effect.fnUntraced(function* (command, options) {
  const commandPath = options?.commandPath ?? [command.name];
  const selected = getCommandAtPath(command, commandPath);
  const commandLine = (options?.prefix ?? commandPath).map(value => commandLineArg(value));
  yield* logCurrentCommand(commandLine);
  yield* promptCommand(selected, commandLine, selected === command ? "ROOT" : selected.name);
  return {
    args: commandLine.map(arg => arg.value),
    displayArgs: commandLine.map(arg => arg.displayValue)
  };
});
const getCommandAtPath = (command, commandPath) => {
  let current = command;
  for (const name of commandPath.slice(1)) {
    const child = current.subcommands.flatMap(group => group.commands).find(candidate => candidate.name === name || candidate.alias === name);
    if (child === undefined) {
      break;
    }
    current = child;
  }
  return current;
};
const promptCommand = /*#__PURE__*/Effect.fnUntraced(function* (command, commandLine, sectionName) {
  const impl = toImpl(command);
  const visibleSubcommands = command.subcommands.flatMap(group => group.commands.filter(child => !child.unlisted));
  const config = visibleSubcommands.length === 0 ? impl.config : impl.contextConfig;
  if (config.flags.length > 0) {
    yield* Console.log(renderSection(sectionName, "FLAGS"));
    for (const param of config.flags) {
      commandLine.push(...(yield* promptParam(param)));
    }
    if (config.arguments.length > 0 || visibleSubcommands.length > 0) {
      yield* logCurrentCommand(commandLine);
    }
  }
  if (config.arguments.length > 0) {
    yield* Console.log(renderSection(sectionName, "ARGUMENTS"));
    for (const param of config.arguments) {
      commandLine.push(...(yield* promptParam(param)));
    }
    if (visibleSubcommands.length > 0) {
      yield* logCurrentCommand(commandLine);
    }
  }
  if (visibleSubcommands.length === 0) {
    return;
  }
  const child = yield* Prompt.run(Prompt.Select({
    message: "Command",
    choices: visibleSubcommands.map(command => ({
      title: command.name,
      value: command,
      ...(command.shortDescription !== undefined ? {
        description: command.shortDescription
      } : command.description !== undefined ? {
        description: command.description
      } : {})
    }))
  }));
  yield* Console.log();
  commandLine.push(commandLineArg(child.name));
  if (hasWizardSteps(child)) {
    yield* logCurrentCommand(commandLine);
  }
  yield* promptCommand(child, commandLine, child.name);
});
const hasWizardSteps = command => {
  const hasVisibleSubcommands = command.subcommands.some(group => group.commands.some(child => !child.unlisted));
  return hasVisibleSubcommands || toImpl(command).config.orderedParams.length > 0;
};
const promptParam = /*#__PURE__*/Effect.fnUntraced(function* (param) {
  const single = Param.getUnderlyingSingleOrThrow(param);
  const metadata = Param.getParamMetadata(param);
  if (metadata.isOptional) {
    const include = yield* Prompt.run(Prompt.Confirm({
      message: `Set ${renderParamLabel(single)}?`,
      initial: false
    }));
    if (!include) {
      yield* Console.log();
      return [];
    }
  }
  const count = !metadata.isVariadic ? 1 : yield* Prompt.run(Prompt.Int({
    message: `${renderParamLabel(single)} count`,
    default: Option.getOrElse(metadata.variadicMin, () => 0),
    min: Option.getOrElse(metadata.variadicMin, () => 0),
    ...(Option.isSome(metadata.variadicMax) ? {
      max: metadata.variadicMax.value
    } : {})
  }));
  const values = [];
  for (let i = 0; i < count; i++) {
    values.push(yield* promptSingle(single));
  }
  const parsed = single.kind === Param.flagKind ? {
    flags: {
      [single.name]: values.map(arg => arg.value)
    },
    arguments: []
  } : {
    flags: {},
    arguments: values.map(arg => arg.value)
  };
  yield* param.parse(parsed);
  yield* Console.log();
  if (single.kind === Param.argumentKind) {
    return values;
  }
  // Inline option-looking values so the lexer cannot interpret them as flags or the -- delimiter.
  return values.flatMap(value => value.value.startsWith("-") && value.value.length > 1 ? [commandLineArg(`--${single.name}=${value.value}`, `--${single.name}=${value.displayValue}`)] : [commandLineArg(`--${single.name}`), value]);
});
const promptSingle = single => {
  const message = renderParamMessage(single);
  switch (single.primitiveType._tag) {
    case "Boolean":
      return Effect.map(Prompt.run(Prompt.Confirm({
        message,
        label: {
          confirm: "true",
          deny: "false"
        },
        placeholder: {
          defaultConfirm: "(T/f)",
          defaultDeny: "(t/F)"
        }
      })), value => commandLineArg(String(value)));
    case "Choice":
      {
        const choices = Primitive.getChoiceKeys(single.primitiveType) ?? [];
        return Effect.map(Prompt.run(Prompt.Select({
          message,
          choices: choices.map(choice => ({
            title: choice,
            value: choice
          }))
        })), commandLineArg);
      }
    case "Date":
      return Effect.map(Prompt.run(Prompt.Date({
        message
      })), date => commandLineArg(date.toISOString()));
    case "Finite":
      return Effect.map(Prompt.run(Prompt.Number({
        message
      })), value => commandLineArg(String(value)));
    case "Int":
      return Effect.map(Prompt.run(Prompt.Int({
        message
      })), value => commandLineArg(String(value)));
    case "Redacted":
      return Effect.map(Prompt.run(Prompt.Password({
        message
      })), value => commandLineArg(Redacted.value(value), "<redacted>"));
    default:
      return Effect.map(Prompt.run(Prompt.String({
        message
      })), commandLineArg);
  }
};
const commandLineArg = (value, displayValue = value) => ({
  value,
  displayValue
});
const formatName = single => single.kind === Param.flagKind ? `--${single.name}` : single.name;
const renderParamMessage = single => renderParamLabel(single);
const renderParamLabel = single => {
  const description = Option.getOrUndefined(single.description)?.trim();
  const label = single.kind === Param.flagKind && description !== undefined && description.length <= 32 ? description : humanize(single.name);
  return single.kind === Param.flagKind ? `${label} (${formatName(single)})` : label;
};
const humanize = name => {
  const words = name.split(/[-_]+/).filter(word => word.length > 0);
  if (words.length === 0) return name;
  return [words[0][0].toUpperCase() + words[0].slice(1), ...words.slice(1)].join(" ");
};
const logCurrentCommand = commandLine => Console.log(renderCommandBlock("Current command", commandLine.map(arg => arg.displayValue), Ansi.magenta));
const renderSection = (commandName, section) => `${Ansi.annotate(commandName.toUpperCase(), Ansi.bold, Ansi.cyanBright)} ${Ansi.annotate("·", Ansi.blackBright)} ${Ansi.annotate(section, Ansi.bold, Ansi.white)}`;
export const renderIntroduction = (name, version, summary) => {
  const title = `${Ansi.annotate(name, Ansi.bold, Ansi.cyanBright)} ${Ansi.annotate(`v${version}`, Ansi.white)} ${Ansi.annotate("· Command wizard", Ansi.bold, Ansi.white)}`;
  return [title, ...(summary === undefined || summary.length === 0 ? [] : [summary]), Ansi.annotate("Build a command interactively. Press Ctrl+C to cancel.", Ansi.blackBright), ""].join("\n");
};
export const renderCompletion = commandLine => renderCommandBlock("Command ready", commandLine, Ansi.cyanBright, Ansi.green);
export const renderQuit = () => `\n${Ansi.annotate("Wizard cancelled.", Ansi.red)}`;
const renderCommandBlock = (label, commandLine, commandColor, labelColor = Ansi.white) => {
  const lines = wrapCommand(commandLine);
  return [Ansi.annotate(label, Ansi.bold, labelColor), ...lines.map(line => Ansi.annotate(line, commandColor)), ""].join("\n");
};
const wrapCommand = commandLine => {
  const width = 88;
  const firstIndent = "  $ ";
  const continuationIndent = "    ";
  const args = commandLine.map(formatShellArg);
  const lines = [];
  let current = firstIndent;
  for (const arg of args) {
    const separator = current === firstIndent || current === continuationIndent ? "" : " ";
    if (current.length + separator.length + arg.length > width && current !== firstIndent) {
      lines.push(`${current} \\`);
      current = `${continuationIndent}${arg}`;
    } else {
      current += `${separator}${arg}`;
    }
  }
  if (current !== firstIndent) {
    lines.push(current);
  }
  return lines;
};
const formatShellArg = arg => /^[A-Za-z0-9_./:@%+=,-]+$/.test(arg) ? arg : `'${arg.replaceAll("'", `'"'"'`)}'`;
//# sourceMappingURL=wizard.js.map