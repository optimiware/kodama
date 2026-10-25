/**
 * Adapts Effect schemas for OpenAI structured output.
 *
 * OpenAI structured output accepts only a subset of JSON Schema. This module
 * converts an Effect `Schema.Codec` into a provider-compatible JSON Schema and
 * a matching codec for decoding the model response back into the original
 * application type. Unsupported constraints can be omitted from the provider
 * schema and remain enforced by the returned codec.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type * as JsonSchema from "../JsonSchema.ts";
import * as Schema from "../Schema.ts";
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
export declare function toCodecOpenAI<T, E, RD, RE>(schema: Schema.ConstraintCodec<T, E, RD, RE>): {
    codec: Schema.ConstraintCodec<T, unknown, RD, RE>;
    jsonSchema: JsonSchema.JsonSchema;
};
//# sourceMappingURL=OpenAiStructuredOutput.d.ts.map