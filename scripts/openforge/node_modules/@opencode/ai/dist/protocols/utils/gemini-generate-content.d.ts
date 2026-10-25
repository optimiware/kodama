import { Effect, Schema, type Stream } from "effect";
import type { Media } from "../../media.js";
import type { MediaProtocol } from "../../route/media-protocol.js";
import { AIError, type MediaUsage, type ProviderMetadata } from "../../schema/index.js";
declare const UsageMetadata: Schema.Struct<{
    readonly promptTokenCount: Schema.optional<Schema.Number>;
    readonly candidatesTokenCount: Schema.optional<Schema.Number>;
    readonly totalTokenCount: Schema.optional<Schema.Number>;
}>;
type UsageMetadata = Schema.Schema.Type<typeof UsageMetadata>;
export declare const chunk: <const Part extends Schema.Top>(part: Part) => Schema.Struct<{
    readonly candidates: Schema.optional<Schema.$Array<Schema.Struct<{
        readonly content: Schema.optional<Schema.Struct<{
            readonly parts: Schema.optional<Schema.$Array<Part>>;
        }>>;
        readonly finishReason: Schema.optional<Schema.String>;
    }>>>;
    readonly promptFeedback: Schema.optional<Schema.Struct<{
        readonly blockReason: Schema.optional<Schema.String>;
        readonly blockReasonMessage: Schema.optional<Schema.String>;
    }>>;
    readonly usageMetadata: Schema.optional<Schema.Struct<{
        readonly promptTokenCount: Schema.optional<Schema.Number>;
        readonly candidatesTokenCount: Schema.optional<Schema.Number>;
        readonly totalTokenCount: Schema.optional<Schema.Number>;
    }>>;
    readonly modelVersion: Schema.optional<Schema.String>;
    readonly responseId: Schema.optional<Schema.String>;
}>;
interface Chunk {
    readonly candidates?: ReadonlyArray<{
        readonly finishReason?: string;
    }>;
    readonly promptFeedback?: {
        readonly blockReason?: string;
        readonly blockReasonMessage?: string;
    };
    readonly usageMetadata?: UsageMetadata;
    readonly modelVersion?: string;
    readonly responseId?: string;
}
export interface Metadata {
    readonly usage?: UsageMetadata;
    readonly finishReason?: string;
    readonly modelVersion?: string;
    readonly responseId?: string;
}
export declare const track: <State extends Metadata>(state: State, chunk: Chunk) => State;
export declare const blocked: (name: string, chunk: Chunk, frame: string) => AIError | undefined;
/** Finish reasons for which Gemini stops output on safety or policy grounds. */
export declare const contentFiltered: (finishReason: string | undefined) => boolean;
/** Callers check that the response produced no output: a policy stop after output is a partial result instead. */
export declare const withheld: (name: string, chunk: Chunk, frame: string) => AIError | undefined;
/** Any finish reason other than `STOP` means the output may be cut short, so it is surfaced rather than dropped. */
export declare const notices: (name: string, state: Metadata) => ReadonlyArray<Media.Notice> | undefined;
export declare const usage: (usage: UsageMetadata | undefined) => MediaUsage | undefined;
export declare const providerMetadata: (state: Metadata) => ProviderMetadata;
export declare const path: (model: string, mode: MediaProtocol.Mode) => string;
export declare const frames: (bytes: Stream.Stream<Uint8Array, AIError>, mode: MediaProtocol.Mode) => Stream.Stream<string, AIError, never>;
export declare const mediaPart: (route: string, asset: Media.Asset) => Effect.Effect<{
    readonly fileData: {
        readonly mimeType: string;
        readonly fileUri: string;
    };
} | {
    readonly inlineData: {
        readonly mimeType: string;
        readonly data: string;
    };
}, AIError>;
export * as GeminiGenerateContent from "./gemini-generate-content.js";
