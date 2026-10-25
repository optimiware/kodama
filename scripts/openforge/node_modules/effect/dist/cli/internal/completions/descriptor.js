/**
 * CommandDescriptor — pure-data representation of a command tree for
 * shell completion generation.
 *
 * @internal
 */
import * as Option from "../../../Option.js";
import * as Param from "../../Param.js";
import * as Primitive from "../../Primitive.js";
import { toImpl } from "../command.js";
// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
const toFlagType = single => {
  const tag = single.primitiveType._tag;
  switch (tag) {
    case "Boolean":
      return {
        _tag: "Boolean"
      };
    case "Int":
      return {
        _tag: "Int"
      };
    case "Finite":
      return {
        _tag: "Finite"
      };
    case "Date":
      return {
        _tag: "Date"
      };
    case "Choice":
      {
        const keys = Primitive.getChoiceKeys(single.primitiveType);
        return {
          _tag: "Choice",
          values: keys ?? []
        };
      }
    case "Path":
      return {
        _tag: "Path",
        pathType: Primitive.getPathType(single.primitiveType) ?? "either"
      };
    case "FileText":
    case "FileParse":
    case "FileSchema":
      return {
        _tag: "Path",
        pathType: "file"
      };
    default:
      return {
        _tag: "String"
      };
  }
};
const toArgumentType = single => {
  const tag = single.primitiveType._tag;
  switch (tag) {
    case "Int":
      return {
        _tag: "Int"
      };
    case "Finite":
      return {
        _tag: "Finite"
      };
    case "Date":
      return {
        _tag: "Date"
      };
    case "Choice":
      {
        const keys = Primitive.getChoiceKeys(single.primitiveType);
        return {
          _tag: "Choice",
          values: keys ?? []
        };
      }
    case "Path":
      return {
        _tag: "Path",
        pathType: Primitive.getPathType(single.primitiveType) ?? "either"
      };
    case "FileText":
    case "FileParse":
    case "FileSchema":
      return {
        _tag: "Path",
        pathType: "file"
      };
    default:
      return {
        _tag: "String"
      };
  }
};
// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------
/**
 * @internal
 */
export const fromCommand = (cmd, inheritedFlags = []) => {
  const impl = toImpl(cmd);
  const config = impl.config;
  const flags = [];
  const seen = new Set();
  // Match help's precedence: local flags first, then ancestors root to leaf.
  for (const [index, flag] of [...config.flags, ...inheritedFlags].entries()) {
    const singles = Param.extractSingleParams(flag);
    for (const single of singles) {
      if (single.kind !== "flag") continue;
      // Omit hidden flags from completion scripts so tab-completion in the
      // shell does not advertise flags that are absent from --help.
      if (single.hidden) continue;
      if (index >= config.flags.length && seen.has(single.name)) continue;
      seen.add(single.name);
      flags.push({
        name: single.name,
        aliases: single.aliases,
        description: Option.getOrUndefined(single.description),
        type: toFlagType(single)
      });
    }
  }
  const args = [];
  for (const arg of config.arguments) {
    const singles = Param.extractSingleParams(arg);
    const metadata = Param.getParamMetadata(arg);
    for (const single of singles) {
      if (single.kind !== "argument") continue;
      args.push({
        name: single.name,
        description: Option.getOrUndefined(single.description),
        required: !metadata.isOptional,
        variadic: metadata.isVariadic,
        type: toArgumentType(single)
      });
    }
  }
  const subcommands = [];
  const sharedFlags = [...inheritedFlags, ...impl.contextConfig.flags];
  for (const group of cmd.subcommands) {
    for (const subcommand of group.commands) {
      // Omit unlisted subcommands from completion scripts so tab-completion in
      // the shell does not advertise commands that are absent from --help.
      if (subcommand.unlisted) continue;
      const descriptor = fromCommand(subcommand, sharedFlags);
      subcommands.push(descriptor);
      if (subcommand.alias && subcommand.alias !== subcommand.name) {
        subcommands.push({
          ...descriptor,
          name: subcommand.alias
        });
      }
    }
  }
  return {
    name: cmd.name,
    description: cmd.shortDescription ?? cmd.description,
    flags,
    arguments: args,
    subcommands
  };
};
//# sourceMappingURL=descriptor.js.map