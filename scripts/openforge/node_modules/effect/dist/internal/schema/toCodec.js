import { memoize } from "../../Function.js";
import * as Predicate from "../../Predicate.js";
import * as SchemaAST from "../../SchemaAST.js";
import * as SchemaGetter from "../../SchemaGetter.js";
import * as InternalTransformation from "../../SchemaTransformation.js";
import * as InternalMake from "./make.js";
/** @internal */
export function toCodecJson(schema) {
  return InternalMake.make(toCodecJsonAST(schema.ast), {
    schema
  });
}
/** @internal */
export const toCodecJsonAST = /*#__PURE__*/SchemaAST.applyToSelfOrLastLinkEncodingIdempotent(ast => {
  const out = toCodecJsonASTStep(ast, toCodecJsonAST);
  const context = ast.context;
  if (out === ast || context === undefined) return out;
  return SchemaAST.replaceContextLastLink(out, withoutConstructorDefault(context));
});
function withoutConstructorDefault(context) {
  return context.constructorDefault === undefined ? context : new SchemaAST.Context(context.isOptional, context.isMutable, undefined, context.annotations);
}
function validateCanonicalObjectPropertyNames(ast) {
  if (ast.propertySignatures.some(ps => typeof ps.name !== "string")) {
    throw new globalThis.Error("Objects property names must be strings", {
      cause: ast
    });
  }
}
function makeReorder(getPriority) {
  return types => {
    const sortedTypes = [...types].sort((a, b) => getPriority(SchemaAST.toEncoded(a)) - getPriority(SchemaAST.toEncoded(b)));
    // Check if order changed by comparing arrays
    const orderChanged = sortedTypes.some((ast, index) => ast !== types[index]);
    if (!orderChanged) return types;
    return sortedTypes;
  };
}
const toCodecJsonReorder = /*#__PURE__*/makeReorder(ast => {
  switch (ast._tag) {
    case "BigInt":
    case "Symbol":
    case "UniqueSymbol":
      return 0;
    default:
      return 1;
  }
});
function toCodecJsonASTStep(ast, recur) {
  switch (ast._tag) {
    case "Declaration":
      {
        const getLink = ast.annotations?.toCodecJson ?? ast.annotations?.toCodec;
        if (!Predicate.isFunction(getLink)) {
          return SchemaAST.replaceEncoding(ast, [SchemaAST.unknownToJson]);
        }
        const typeParameters = ast.typeParameters.map(tp => InternalMake.make(SchemaAST.toEncoded(tp)));
        const link = getLink(typeParameters);
        return link === undefined ? ast : SchemaAST.replaceEncoding(ast, [SchemaAST.mapLink(link, recur)]);
      }
    case "Unknown":
      return SchemaAST.replaceEncoding(ast, [SchemaAST.unknownToJson]);
    case "ObjectKeyword":
      return SchemaAST.replaceEncoding(ast, [SchemaAST.objectKeywordToJson]);
    case "Undefined":
    case "Void":
    case "Literal":
    case "Number":
      return ast.toCodecJson();
    case "UniqueSymbol":
    case "Symbol":
    case "BigInt":
      return ast.toCodecStringTree();
    case "Objects":
      {
        validateCanonicalObjectPropertyNames(ast);
        return ast.recur(recur, SchemaAST.parameterFromString);
      }
    case "Union":
      {
        const sortedTypes = toCodecJsonReorder(ast.types);
        if (sortedTypes !== ast.types) {
          return new SchemaAST.Union(sortedTypes, ast.options, ast.annotations, ast.checks, ast.encoding, ast.context, ast.encodingChecks).recur(recur);
        }
        return ast.recur(recur);
      }
    case "Arrays":
    case "Suspend":
      return ast.recur(recur);
  }
  // `Schema.Any` is used as an escape hatch
  return ast;
}
/** @internal */
export function toCodecIso(schema) {
  return InternalMake.make(toCodecIsoAST(SchemaAST.toType(schema.ast)));
}
const toCodecIsoAST = /*#__PURE__*/memoize(ast => {
  const out = toCodecIsoASTStep(ast, toCodecIsoAST);
  return out !== ast && ast.context !== undefined ? SchemaAST.replaceContextLastLink(out, withoutConstructorDefault(ast.context)) : out;
});
function toCodecIsoASTStep(ast, recur) {
  switch (ast._tag) {
    case "Declaration":
      {
        const getLink = ast.annotations?.toCodecIso ?? ast.annotations?.toCodec;
        if (Predicate.isFunction(getLink)) {
          const link = getLink(ast.typeParameters.map(tp => InternalMake.make(tp)));
          return SchemaAST.replaceEncoding(ast, [SchemaAST.mapLink(link, recur)]);
        }
        return ast;
      }
    case "Arrays":
    case "Objects":
    case "Union":
    case "Suspend":
      return ast.recur(recur);
  }
  return ast;
}
/** @internal */
export function toCodecStringTree(schema) {
  return InternalMake.make(toCodecStringTreeAST(schema.ast), {
    schema
  });
}
/** @internal */
export function toCodecArrayFromSingle(schema) {
  return InternalMake.make(toCodecArrayFromSingleAST(schema.ast));
}
const toStringTreeReorder = /*#__PURE__*/makeReorder(ast => {
  switch (ast._tag) {
    case "Null":
    case "Boolean":
    case "Number":
    case "BigInt":
    case "Symbol":
    case "UniqueSymbol":
      return 0;
    default:
      return 1;
  }
});
function toCodecStringTreeASTStep(ast, recur, onMissingAnnotation) {
  switch (ast._tag) {
    case "Declaration":
      {
        const typeParameters = ast.typeParameters.map(tp => InternalMake.make(recur(SchemaAST.toEncoded(tp))));
        const getStringTreeLink = ast.annotations?.toCodecStringTree;
        if (Predicate.isFunction(getStringTreeLink)) {
          const link = getStringTreeLink(typeParameters);
          if (link === undefined) return ast;
          return SchemaAST.replaceEncoding(ast, [SchemaAST.mapLink(link, recur)]);
        }
        const getJsonLink = ast.annotations?.toCodecJson;
        const jsonLink = Predicate.isFunction(getJsonLink) ? getJsonLink(typeParameters) : undefined;
        const getLink = jsonLink === undefined ? ast.annotations?.toCodec : undefined;
        const link = jsonLink ?? (Predicate.isFunction(getLink) ? getLink(typeParameters) : undefined);
        return link === undefined ? onMissingAnnotation(ast) : SchemaAST.replaceEncoding(ast, [SchemaAST.mapLink(link, recur)]);
      }
    case "Null":
      return SchemaAST.replaceEncoding(ast, [nullToString]);
    case "Boolean":
      return SchemaAST.replaceEncoding(ast, [booleanToString]);
    case "Unknown":
    case "ObjectKeyword":
      return SchemaAST.replaceEncoding(ast, [SchemaAST.unknownToStringTree]);
    case "Enum":
    case "Number":
    case "Literal":
    case "UniqueSymbol":
    case "Symbol":
    case "BigInt":
      return ast.toCodecStringTree();
    case "Objects":
      {
        validateCanonicalObjectPropertyNames(ast);
        return ast.recur(recur, SchemaAST.parameterFromString);
      }
    case "Union":
      {
        const sortedTypes = toStringTreeReorder(ast.types);
        if (sortedTypes !== ast.types) {
          return new SchemaAST.Union(sortedTypes, ast.options, ast.annotations, ast.checks, ast.encoding, ast.context, ast.encodingChecks).recur(recur);
        }
        return ast.recur(recur);
      }
    case "Arrays":
    case "Suspend":
      return ast.recur(recur);
  }
  // `Schema.Any` is used as an escape hatch
  return ast;
}
const nullToString = /*#__PURE__*/new SchemaAST.Link(/*#__PURE__*/new SchemaAST.Literal("null"), /*#__PURE__*/new InternalTransformation.Transformation(/*#__PURE__*/SchemaGetter.transform(() => null), /*#__PURE__*/SchemaGetter.transform(() => "null")));
const booleanToString = /*#__PURE__*/new SchemaAST.Link(/*#__PURE__*/new SchemaAST.Union([/*#__PURE__*/new SchemaAST.Literal("true"), /*#__PURE__*/new SchemaAST.Literal("false")]), /*#__PURE__*/new InternalTransformation.Transformation(/*#__PURE__*/SchemaGetter.transform(s => s === "true"), /*#__PURE__*/SchemaGetter.String()));
const arrayFromSingleTransformation = /*#__PURE__*/new InternalTransformation.Transformation(/*#__PURE__*/SchemaGetter.transform(input => typeof input === "string" ? [input] : input), /*#__PURE__*/SchemaGetter.passthrough());
const isCodecArrayFromSingleLink = link => link.transformation === arrayFromSingleTransformation;
const toCodecStringTreeAST = /*#__PURE__*/SchemaAST.applyToSelfOrLastLinkEncodingIdempotent(ast => {
  const out = toCodecStringTreeASTStep(ast, toCodecStringTreeAST, ast => {
    throw new globalThis.Error("Missing structural codec for StringTree", {
      cause: ast
    });
  });
  if (out !== ast && ast.context !== undefined) {
    return SchemaAST.replaceContextLastLink(out, withoutConstructorDefault(ast.context));
  }
  return out;
}, {
  stopAt: isCodecArrayFromSingleLink
});
const toArrayFromSingleInputElement = ast => SchemaAST.isOptional(ast) ? SchemaAST.optionalKey(SchemaAST.unknown) : SchemaAST.unknown;
const toCodecArrayFromSingleAST = /*#__PURE__*/SchemaAST.applyToSelfOrLastLinkEncodingIdempotent(ast => {
  const out = toCodecArrayFromSingleASTStep(ast);
  if (SchemaAST.isArrays(out)) {
    const ensure = SchemaAST.decodeTo(new SchemaAST.Union([new SchemaAST.Arrays(out.isMutable, out.elements.map(toArrayFromSingleInputElement), out.rest.map(toArrayFromSingleInputElement)), SchemaAST.string], {
      mode: "anyOf"
    }), out, arrayFromSingleTransformation);
    return SchemaAST.isOptional(ast) ? SchemaAST.optionalKey(ensure) : ensure;
  }
  return out;
}, {
  stopAt: isCodecArrayFromSingleLink
});
function toCodecArrayFromSingleASTStep(ast) {
  return ast._tag === "Declaration" || ast._tag === "Arrays" || ast._tag === "Objects" || ast._tag === "Union" || ast._tag === "Suspend" ? ast.recur(toCodecArrayFromSingleAST) : ast;
}
//# sourceMappingURL=toCodec.js.map