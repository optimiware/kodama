import * as Cause from "../../Cause.js";
import * as Effect from "../../Effect.js";
import * as SchemaAST from "../../SchemaAST.js";
import * as SchemaIssue from "../../SchemaIssue.js";
import { effectIsExit } from "../effect.js";
import * as InternalParser from "./parser.js";
const flatMapTransformation = (result, current, f) => result === InternalParser.sameExit ? f(current) : Effect.flatMapEager(result, f);
function compileTransformation(transformation) {
  if (transformation._tag === "Middleware") {
    return (result, current, options) => {
      const transformed = result === InternalParser.sameExit ? transformation.decode(InternalParser.succeed(InternalParser.toOption(current)), options) : transformation.decode(Effect.mapEager(result, InternalParser.toOption), options);
      return fromOptionalEffect(transformed);
    };
  }
  const getter = transformation.decode;
  switch (getter._tag) {
    case "Passthrough":
      return (result, current) => result === InternalParser.sameExit ? InternalParser.succeed(current) : result;
    case "Transform":
      {
        const transform = value => value === InternalParser.missing ? InternalParser.missingExit : InternalParser.succeed(getter.transform(value));
        return (result, current) => flatMapTransformation(result, current, transform);
      }
    case "TransformOptional":
      {
        const transform = value => InternalParser.fromOptionExit(getter.transform(InternalParser.toOption(value)));
        return (result, current) => flatMapTransformation(result, current, transform);
      }
    case "TransformEffect":
      return (result, current, options) => flatMapTransformation(result, current, value => value === InternalParser.missing ? InternalParser.missingExit : getter.transform(value, options));
    case "TransformOptionalEffect":
      return (result, current, options) => flatMapTransformation(result, current, value => fromOptionalEffect(getter.transform(InternalParser.toOption(value), options)));
  }
}
const fromOptionalEffect = effect => Effect.flatMapEager(effect, InternalParser.fromOptionExit);
/** @internal */
export const wrapEncoding = (ast, input, options, effect) => Effect.catchCause(effect, cause => Effect.failCauseSync(() => Cause.map(cause, issue => new SchemaIssue.Encoding(ast, issue, input, options))));
function makeConstructorParser(descriptor, compile) {
  const transform = compileTransformation(descriptor.link.transformation);
  let sourceParser;
  return (input, options) => {
    if (input === InternalParser.missing) return InternalParser.missingExit;
    if (descriptor.isConstructed(input)) return InternalParser.sameExit;
    const result = (sourceParser ??= compile(descriptor.link.to))(input, options);
    return transform(result, input, options);
  };
}
function withDefault(ast, parser) {
  const defaultValue = ast.context.constructorDefault;
  return (input, options) => {
    if (input !== InternalParser.missing && input !== undefined) return parser(input, options);
    const result = defaultValue;
    if (effectIsExit(result) && result._tag === "Success") {
      const local = parser(result[InternalParser.args], options);
      return local === InternalParser.sameExit ? result : local;
    }
    return Effect.flatMapEager(wrapEncoding(ast, input, options, result), value => {
      const local = parser(value, options);
      return local === InternalParser.sameExit ? InternalParser.succeed(value) : local;
    });
  };
}
/** @internal */
export function compileField(ast, compile) {
  const parser = compile(ast);
  return ast.context?.constructorDefault === undefined ? parser : withDefault(ast, parser);
}
/** @internal */
export function compile(ast, compile, compileField, base, specialize) {
  if (ast._tag === "Declaration") {
    // Declaration callbacks can create public parsers for their type parameters.
    // Register those ASTs with the same resolver before invoking the callback.
    for (const parameter of ast.typeParameters) compile(parameter);
  }
  // Construction supplies compileField for parent-owned defaults. Its presence
  // also selects the constructor semantics of declarations and Unions.
  const descriptor = compileField ? SchemaAST.getConstructorDescriptor(ast) : undefined;
  const parser = descriptor ? makeConstructorParser(descriptor, compile) : base ?? ast.getParser(compile, compileField);
  const checks = ast.checks;
  const links = ast.encoding;
  const transformations = links?.map(link => compileTransformation(link.transformation));
  const encodingChecks = ast.encodingChecks;
  if (!links && !checks && !encodingChecks) {
    return parser;
  }
  let encodingParsers;
  const parseChecks = (input, options) => {
    let result = parser(input, options);
    if (encodingChecks && !options.disableChecks) {
      if (effectIsExit(result)) {
        if (result._tag === "Success") {
          const output = result === InternalParser.sameExit ? input : result[InternalParser.args];
          if (input !== InternalParser.missing && output !== InternalParser.missing) {
            const issues = SchemaAST.collectIssues(encodingChecks, input, undefined, ast, options);
            if (issues) {
              result = Effect.fail(new SchemaIssue.Composite(ast, issues, input, options));
            }
          }
        }
      } else {
        result = Effect.flatMap(result, value => {
          if (input !== InternalParser.missing && value !== InternalParser.missing) {
            const issues = SchemaAST.collectIssues(encodingChecks, input, undefined, ast, options);
            if (issues) {
              return Effect.fail(new SchemaIssue.Composite(ast, issues, input, options));
            }
          }
          return Effect.succeed(value);
        });
      }
    }
    if (checks && !options.disableChecks) {
      if (effectIsExit(result)) {
        if (result._tag === "Success") {
          const value = result === InternalParser.sameExit ? input : result[InternalParser.args];
          if (value === InternalParser.missing) return result;
          const issues = SchemaAST.collectIssues(checks, value, undefined, ast, options);
          if (issues) {
            result = Effect.fail(new SchemaIssue.Composite(ast, issues, value, options));
          }
        }
      } else {
        result = Effect.flatMap(result, value => {
          if (value !== InternalParser.missing) {
            const issues = SchemaAST.collectIssues(checks, value, undefined, ast, options);
            if (issues) {
              return Effect.fail(new SchemaIssue.Composite(ast, issues, value, options));
            }
          }
          return Effect.succeed(value);
        });
      }
    }
    return result;
  };
  const parseLocal = specialize === undefined ? parseChecks : specialize(parseChecks);
  if (!links) {
    return parseLocal;
  }
  return (input, options) => {
    const parsers = encodingParsers ??= links.map(link => compile(link.to));
    let current = input;
    let result = parsers[parsers.length - 1](input, options);
    for (let i = links.length - 1; i >= 0; i--) {
      result = transformations[i](result, current, options);
      if (i !== 0) {
        const next = parsers[i - 1];
        if (result._tag === "Success") {
          current = result[InternalParser.args];
          result = next(current, options);
        } else {
          result = Effect.flatMapEager(result, value => {
            const nextResult = next(value, options);
            return nextResult === InternalParser.sameExit ? InternalParser.succeed(value) : nextResult;
          });
        }
      }
    }
    if (result._tag === "Success") {
      const value = result[InternalParser.args];
      const local = parseLocal(value, options);
      return local === InternalParser.sameExit ? result : local;
    }
    result = wrapEncoding(ast, input, options, result);
    return Effect.flatMapEager(result, value => {
      const local = parseLocal(value, options);
      return local === InternalParser.sameExit ? InternalParser.succeed(value) : local;
    });
  };
}
//# sourceMappingURL=interpreter.js.map