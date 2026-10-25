import { Schema } from "effect";
import { ReasoningEffort, ReasoningEfforts, type LLMRequest } from "../../schema/index.js";
export { ReasoningEffort, ReasoningEfforts };
export declare const TextVerbosities: readonly ["low", "medium", "high"];
export type TextVerbosity = (typeof TextVerbosities)[number] | (string & {});
export declare const TextVerbosity: Schema.declare<TextVerbosity, TextVerbosity>;
export declare const ResponseIncludables: readonly ["file_search_call.results", "web_search_call.results", "web_search_call.action.sources", "message.input_image.image_url", "computer_call_output.output.image_url", "code_interpreter_call.outputs", "reasoning.encrypted_content", "message.output_text.logprobs"];
export type ResponseIncludable = (typeof ResponseIncludables)[number] | (string & {});
export declare const ServiceTiers: readonly ["auto", "default", "flex", "priority"];
export type ServiceTier = (typeof ServiceTiers)[number] | (string & {});
export declare const ServiceTier: Schema.declare<ServiceTier, ServiceTier>;
export declare const Truncations: readonly ["auto", "disabled"];
export type Truncation = (typeof Truncations)[number];
export declare const TextVerbositySchema: Schema.declare<TextVerbosity, TextVerbosity>;
export declare const ResponseIncludableSchema: Schema.declare<ResponseIncludable, ResponseIncludable>;
export declare const ServiceTierSchema: Schema.declare<ServiceTier, ServiceTier>;
export declare const TruncationSchema: Schema.Literals<readonly ["auto", "disabled"]>;
export declare const AllowedTools: Schema.Struct<{
    readonly toolNames: Schema.$Array<Schema.String>;
    readonly mode: Schema.optional<Schema.Literals<readonly ["auto", "none", "required"]>>;
}>;
export type AllowedTools = typeof AllowedTools.Type;
export declare const StreamOptions: Schema.Struct<{
    readonly includeObfuscation: Schema.optional<Schema.Boolean>;
}>;
export declare const Options: Schema.Struct<{
    readonly store: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Boolean>, never>>;
    readonly metadata: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.$Record<Schema.String, Schema.String>>, never>>;
    readonly safetyIdentifier: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.String>, never>>;
    readonly streamOptions: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Struct<{
        readonly includeObfuscation: Schema.optional<Schema.Boolean>;
    }>>, never>>;
    readonly topLogprobs: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Int>, never>>;
    readonly reasoningEffort: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.declare<ReasoningEffort, ReasoningEffort>>, never>>;
    readonly reasoningSummary: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Literals<readonly ["auto", "concise", "detailed"]>>, never>>;
    readonly include: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.$Array<Schema.declare<ResponseIncludable, ResponseIncludable>>>, never>>;
    readonly textVerbosity: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.declare<TextVerbosity, TextVerbosity>>, never>>;
    readonly serviceTier: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.declare<ServiceTier, ServiceTier>>, never>>;
    readonly truncation: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Literals<readonly ["auto", "disabled"]>>, never>>;
    readonly allowedTools: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Struct<{
        readonly toolNames: Schema.$Array<Schema.String>;
        readonly mode: Schema.optional<Schema.Literals<readonly ["auto", "none", "required"]>>;
    }>>, never>>;
    readonly maxToolCalls: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Int>, never>>;
    readonly parallelToolCalls: Schema.optionalKey<Schema.middlewareDecoding<Schema.UndefinedOr<Schema.Boolean>, never>>;
}>;
export type Options = typeof Options.Type;
export type Resolved = Omit<Options, "allowedTools"> & {
    readonly allowedTools?: AllowedTools & {
        readonly mode: NonNullable<AllowedTools["mode"]>;
    };
};
export declare const resolve: (request: LLMRequest) => Resolved;
export * as OpenResponsesOptions from "./open-responses-options.js";
