import * as Rec from "../Record.js";
import * as Schema from "../Schema.js";
import * as InternalStructuredOutput from "./internal/structured-output.js";
import * as LanguageModel from "./LanguageModel.js";
import * as OpenAiStructuredOutput from "./OpenAiStructuredOutput.js";
/**
 * Converts a `Schema.Codec` to Anthropic structured-output JSON Schema and a
 * matching codec for model output.
 *
 * **When to use**
 *
 * Use when you send Effect Schema-backed structured output requests to
 * Anthropic and need provider-compatible JSON Schema without losing the decoded
 * application type.
 *
 * **Details**
 *
 * Returns the JSON Schema to include in the request and the codec to use when
 * decoding the model response. The codec remains authoritative: the provider
 * JSON Schema can be a lossy, less restrictive representation when Anthropic
 * cannot express an Effect Schema constraint.
 *
 * **Gotchas**
 *
 * - Some schemas use a provider-safe encoded shape: tuples become objects with
 *   numeric string keys, objects with index signatures become arrays of
 *   `[key, value]` pairs, and optional properties become required nullable
 *   properties.
 * - `oneOf` unions are emitted as `anyOf` unions.
 * - Unsupported constraints are removed from the provider schema and are still
 *   checked while decoding with the returned codec.
 * - Recursive schemas throw during conversion because Anthropic structured
 *   output does not support recursive references.
 *
 * @see {@link LanguageModel.CodecTransformer} for the structured-output transformer contract
 * @see {@link OpenAiStructuredOutput.toCodecOpenAI} for the OpenAI-specific transformer
 *
 * @stability unstable
 * @category transforming
 * @since 4.0.0
 */
export function toCodecAnthropic(schema) {
  const codec = InternalStructuredOutput.toCodec(schema);
  const unresolvedDocument = Schema.toJsonSchemaDocument(codec, {
    generateDescriptions: true,
    onExcessProperty: "error"
  });
  if (hasReferenceCycle(unresolvedDocument.schema, unresolvedDocument.definitions)) {
    throw new Error("AnthropicStructuredOutput: Recursive schemas are not supported");
  }
  const document = InternalStructuredOutput.resolveTopLevelReference(unresolvedDocument);
  const jsonSchema = rewriteAnthropic(document.schema);
  if (Object.keys(document.definitions).length > 0) {
    jsonSchema.$defs = Rec.map(document.definitions, rewriteAnthropic);
  }
  return {
    codec,
    jsonSchema
  };
}
function hasReferenceCycle(root, definitions) {
  const visiting = new Set();
  const visited = new Set();
  function visit(schema) {
    if (visiting.has(schema)) return true;
    if (visited.has(schema)) return false;
    visiting.add(schema);
    let cycle = false;
    InternalStructuredOutput.walkJsonSchema(schema, node => {
      if (!cycle && typeof node.$ref === "string") {
        const target = node.$ref === "#" ? root : InternalStructuredOutput.resolveReference(node.$ref, definitions);
        if (visit(target)) cycle = true;
      }
      return node;
    });
    visiting.delete(schema);
    visited.add(schema);
    return cycle;
  }
  return visit(root);
}
function rewriteAnthropic(schema) {
  return InternalStructuredOutput.walkJsonSchema(schema, schema => {
    const normalized = hoistAllOfDescriptions(schema);
    const out = {};
    let unsupportedFormat;
    for (const [key, value] of Object.entries(normalized)) {
      if (key === "format") {
        if (typeof value === "string" && formats.has(value) && (normalized.type === undefined || normalized.type === "string")) {
          out.format = value;
        } else if (typeof value === "string") unsupportedFormat = value;
      } else if (supportedKeywords.has(key)) {
        if (key !== "additionalProperties" || value === false) out[key] = value;
      }
    }
    if (unsupportedFormat !== undefined) {
      InternalStructuredOutput.appendDescription(out, `a value with a format of ${unsupportedFormat}`);
    }
    return out;
  });
}
function hoistAllOfDescriptions(schema) {
  if (!Array.isArray(schema.allOf)) return schema;
  const out = {};
  for (const [key, value] of Object.entries(schema)) {
    if (key !== "allOf") out[key] = value;
  }
  const members = [];
  for (const member of schema.allOf) {
    if (!InternalStructuredOutput.isJsonSchema(member)) continue;
    const {
      description,
      format,
      title,
      ...memberRest
    } = member;
    const rest = {
      ...memberRest
    };
    if (schema.type !== undefined && schema.type !== "string") delete rest.pattern;
    if (typeof description === "string") {
      InternalStructuredOutput.appendDescription(out, description);
    }
    if (out.format === undefined && typeof format === "string") out.format = format;
    if (out.title === undefined && typeof title === "string") out.title = title;
    if (Object.keys(rest).length > 0) members.push(rest);
  }
  if (members.length > 0) out.allOf = members;
  return out;
}
const supportedKeywords = /*#__PURE__*/new Set(["$ref", "type", "title", "description", "enum", "const", "anyOf", "allOf", "properties", "required", "additionalProperties", "items", "pattern"]);
const formats = /*#__PURE__*/new Set(["date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid"]);
//# sourceMappingURL=AnthropicStructuredOutput.js.map