/**
 * Adapts Effect Schema codecs to the JSON Schema subset accepted by Anthropic
 * structured output.
 *
 * The main entry point returns the JSON Schema to send to Anthropic and a codec
 * for decoding the model response back into the original application type.
 * Unsupported constraints can be omitted from the provider schema and remain
 * enforced by the returned codec.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type * as JsonSchema from "../JsonSchema.ts";
import * as Schema from "../Schema.ts";
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
export declare function toCodecAnthropic<T, E, RD, RE>(schema: Schema.ConstraintCodec<T, E, RD, RE>): {
    readonly codec: Schema.ConstraintCodec<T, unknown, RD, RE>;
    readonly jsonSchema: JsonSchema.JsonSchema;
};
//# sourceMappingURL=AnthropicStructuredOutput.d.ts.map