import * as Rec from "../Record.js";
import * as Schema from "../Schema.js";
import * as InternalStructuredOutput from "./internal/structured-output.js";
/**
 * Converts a `Schema.Codec` to OpenAI structured-output JSON Schema and a
 * matching codec for model output.
 *
 * **When to use**
 *
 * Use when you send Effect Schema-backed structured output requests to OpenAI
 * standard models and need provider-compatible JSON Schema without losing the
 * decoded application type.
 *
 * **Details**
 *
 * Returns the JSON Schema to include in the request and the codec to use when
 * decoding the model response. The codec remains authoritative: the provider
 * JSON Schema can be a lossy, less restrictive representation when OpenAI
 * cannot express an Effect Schema constraint. Conversion throws when the
 * resulting root is not an object or contains `anyOf`.
 *
 * **Gotchas**
 *
 * - Some schemas use a provider-safe encoded shape: tuples become objects with
 *   numeric string keys, objects with index signatures become arrays of
 *   `[key, value]` pairs, and optional properties become required nullable
 *   properties.
 * - `oneOf` unions are emitted as `anyOf` unions.
 * - Compatible regex patterns are merged because OpenAI structured output does
 *   not support `allOf`.
 * - The root JSON Schema must be an object and cannot use `anyOf`.
 * - Constraints inside `allOf` are retained only when they have an explicit,
 *   semantics-preserving normalization rule.
 * - Structural constraints inside `allOf`, such as `properties`, `required`,
 *   `additionalProperties`, and `items`, are omitted instead of being merged
 *   with a different meaning.
 * - Unsupported constraints are removed from the provider schema and are still
 *   checked while decoding with the returned codec.
 * - Compatibility targets standard OpenAI models. Fine-tuned models support a
 *   smaller JSON Schema subset.
 *
 * @stability unstable
 * @category transforming
 * @since 4.0.0
 */
export function toCodecOpenAI(schema) {
  const codec = InternalStructuredOutput.toCodec(schema);
  const document = InternalStructuredOutput.resolveTopLevelReference(Schema.toJsonSchemaDocument(codec, {
    generateDescriptions: true,
    onExcessProperty: "error"
  }));
  const jsonSchema = rewriteOpenAI(document.schema);
  if (jsonSchema.type !== "object" || jsonSchema.anyOf !== undefined) {
    throw new Error(`OpenAiStructuredOutput: Root JSON Schema must have type "object" and must not use "anyOf"`);
  }
  if (Object.keys(document.definitions).length > 0) {
    jsonSchema.$defs = Rec.map(document.definitions, rewriteOpenAI);
  }
  return {
    codec,
    jsonSchema
  };
}
function rewriteOpenAI(schema) {
  return InternalStructuredOutput.walkJsonSchema(schema, schema => {
    const normalized = normalizeAllOf(schema);
    const out = {};
    let unsupportedFormat;
    for (const [key, value] of Object.entries(normalized)) {
      if (key === "format") {
        if (typeof value === "string" && formats.has(value) && supportsType(key, normalized.type)) out.format = value;else if (typeof value === "string") unsupportedFormat = value;
      } else if (supportedKeywords.has(key) && supportsType(key, normalized.type)) {
        if (key !== "additionalProperties" || value === false) out[key] = value;
      }
    }
    if (unsupportedFormat !== undefined) {
      InternalStructuredOutput.appendDescription(out, `a value with a format of ${unsupportedFormat}`);
    }
    if (out.type === "object" && out.properties === undefined && out.additionalProperties === false) {
      out.properties = {};
    }
    return out;
  });
}
function supportsType(key, type) {
  if (typeof type !== "string") return true;
  switch (key) {
    case "pattern":
    case "format":
      return type === "string";
    case "multipleOf":
    case "minimum":
    case "exclusiveMinimum":
    case "maximum":
    case "exclusiveMaximum":
      return type === "number" || type === "integer";
    case "items":
    case "minItems":
    case "maxItems":
      return type === "array";
    case "properties":
    case "patternProperties":
    case "required":
    case "additionalProperties":
      return type === "object";
    default:
      return true;
  }
}
function normalizeAllOf(schema) {
  if (!Array.isArray(schema.allOf)) return schema;
  const out = {};
  const patterns = [];
  const baseKeywords = new Set();
  const memberKeywords = new Set();
  const ambiguousKeywords = new Set();
  for (const [key, value] of Object.entries(schema)) {
    if (key === "allOf") continue;
    baseKeywords.add(key);
    if (key === "pattern" && typeof value === "string") patterns.push(value);else out[key] = value;
  }
  for (const member of schema.allOf) {
    if (!InternalStructuredOutput.isJsonSchema(member)) continue;
    for (const [key, value] of Object.entries(member)) {
      mergeAllOfKeyword(out, key, value, patterns, baseKeywords, memberKeywords, ambiguousKeywords);
    }
  }
  const uniquePatterns = Array.from(new Set(patterns));
  if (uniquePatterns.length === 1) {
    out.pattern = uniquePatterns[0];
  } else if (uniquePatterns.length > 1) {
    const combined = uniquePatterns.map(source => `(?=[\\s\\S]*?(?:${source}))`).join("");
    out.pattern = `^${combined}`;
  }
  return out;
}
function mergeAllOfKeyword(out, key, value, patterns, baseKeywords, memberKeywords, ambiguousKeywords) {
  switch (key) {
    case "description":
      if (typeof value === "string") InternalStructuredOutput.appendDescription(out, value);
      return;
    case "pattern":
      if (typeof value === "string") patterns.push(value);
      return;
    case "minimum":
    case "exclusiveMinimum":
      mergeLowerBound(out, key, value);
      return;
    case "maximum":
    case "exclusiveMaximum":
      mergeUpperBound(out, key, value);
      return;
    case "minItems":
      mergeMinimum(out, key, value);
      return;
    case "maxItems":
      mergeMaximum(out, key, value);
      return;
    default:
      if (!normalizableAllOfKeywords.has(key) || baseKeywords.has(key) || ambiguousKeywords.has(key)) return;
      if (!memberKeywords.has(key)) {
        out[key] = value;
        memberKeywords.add(key);
      } else if (out[key] !== value) {
        delete out[key];
        memberKeywords.delete(key);
        ambiguousKeywords.add(key);
      }
      return;
  }
}
function mergeMinimum(schema, key, value) {
  if (typeof value !== "number") return;
  const current = schema[key];
  if (typeof current !== "number" || value > current) schema[key] = value;
}
function mergeMaximum(schema, key, value) {
  if (typeof value !== "number") return;
  const current = schema[key];
  if (typeof current !== "number" || value < current) schema[key] = value;
}
function mergeLowerBound(schema, key, value) {
  if (typeof value !== "number") {
    if (!Object.hasOwn(schema, key)) schema[key] = value;
    return;
  }
  const minimum = typeof schema.minimum === "number" ? schema.minimum : undefined;
  const exclusiveMinimum = typeof schema.exclusiveMinimum === "number" ? schema.exclusiveMinimum : undefined;
  let current;
  if (minimum !== undefined) current = {
    value: minimum,
    exclusive: false
  };
  if (exclusiveMinimum !== undefined && (current === undefined || exclusiveMinimum >= current.value)) {
    current = {
      value: exclusiveMinimum,
      exclusive: true
    };
  }
  const candidate = {
    value,
    exclusive: key === "exclusiveMinimum"
  };
  if (current === undefined || candidate.value > current.value || candidate.value === current.value && candidate.exclusive) {
    current = candidate;
  }
  delete schema.minimum;
  delete schema.exclusiveMinimum;
  schema[current.exclusive ? "exclusiveMinimum" : "minimum"] = current.value;
}
function mergeUpperBound(schema, key, value) {
  if (typeof value !== "number") {
    if (!Object.hasOwn(schema, key)) schema[key] = value;
    return;
  }
  const maximum = typeof schema.maximum === "number" ? schema.maximum : undefined;
  const exclusiveMaximum = typeof schema.exclusiveMaximum === "number" ? schema.exclusiveMaximum : undefined;
  let current;
  if (maximum !== undefined) current = {
    value: maximum,
    exclusive: false
  };
  if (exclusiveMaximum !== undefined && (current === undefined || exclusiveMaximum <= current.value)) {
    current = {
      value: exclusiveMaximum,
      exclusive: true
    };
  }
  const candidate = {
    value,
    exclusive: key === "exclusiveMaximum"
  };
  if (current === undefined || candidate.value < current.value || candidate.value === current.value && candidate.exclusive) {
    current = candidate;
  }
  delete schema.maximum;
  delete schema.exclusiveMaximum;
  schema[current.exclusive ? "exclusiveMaximum" : "maximum"] = current.value;
}
const supportedKeywords = /*#__PURE__*/new Set(["$ref", "type", "title", "description", "enum", "anyOf", "properties", "required", "additionalProperties", "items", "pattern", "multipleOf", "minimum", "exclusiveMinimum", "maximum", "exclusiveMaximum", "minItems", "maxItems"]);
const normalizableAllOfKeywords = /*#__PURE__*/new Set(["$ref", "title", "enum", "anyOf", "format", "multipleOf"]);
const formats = /*#__PURE__*/new Set(["date-time", "time", "date", "duration", "email", "hostname", "ipv4", "ipv6", "uuid"]);
//# sourceMappingURL=OpenAiStructuredOutput.js.map