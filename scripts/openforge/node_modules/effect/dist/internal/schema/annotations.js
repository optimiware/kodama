import { memoize } from "../../Function.js";
/** @internal */
export function resolve(ast) {
  return ast.checks ? ast.checks[ast.checks.length - 1].annotations : ast.annotations;
}
/** @internal */
export function resolveAt(key) {
  return ast => resolve(ast)?.[key];
}
/** @internal */
export const STRUCTURAL_ANNOTATION_KEY = "~structural";
/** @internal */
export const IDENTIFIER_FALLBACK_KEY = "~identifier";
/** @internal */
export const SENTINELS_ANNOTATION_KEY = "~sentinels";
/** @internal */
export const CONSTRUCTOR_ANNOTATION_KEY = "~constructor";
/** @internal */
export const jsonSchemaAnnotationKeys = ["title", "description", "default", "examples", "readOnly", "writeOnly", "format", "contentEncoding", "contentMediaType", "contentSchema"];
/** @internal */
export const resolveIdentifier = /*#__PURE__*/resolveAt("identifier");
/** @internal */
export const resolveIdentifierFallback = /*#__PURE__*/resolveAt(IDENTIFIER_FALLBACK_KEY);
/** @internal */
export const resolveTitle = /*#__PURE__*/resolveAt("title");
/** @internal */
export const resolveDescription = /*#__PURE__*/resolveAt("description");
/** @internal */
export const getExpected = /*#__PURE__*/memoize(ast => {
  const identifier = resolve(ast)?.identifier;
  if (typeof identifier === "string") return identifier;
  return ast.getExpected(getExpected);
});
/** @internal */
export const annotationExcludedKeys = /*#__PURE__*/new Set([SENTINELS_ANNOTATION_KEY, STRUCTURAL_ANNOTATION_KEY, "representation", "arbitraryConstraint", "toJsonSchema", "toCode", "toEquivalence", "toFormatter", "toCodec", "toCodecJson", "toCodecStringTree", "toCodecIso", "toCodecArbitrary"]);
//# sourceMappingURL=annotations.js.map