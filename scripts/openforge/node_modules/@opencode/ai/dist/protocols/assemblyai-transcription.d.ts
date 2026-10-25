import { Schema } from "effect";
import { MediaProtocol } from "../route/media-protocol.js";
import { MediaRoute } from "../route/media.js";
import { type OpenString } from "../schema/index.js";
import { TranscriptionModel, TranscriptionResponse, type TranscriptionRequestFor } from "../transcription.js";
export declare const DEFAULT_BASE_URL = "https://api.assemblyai.com";
export declare const PATH = "/v2/transcript";
export declare const UPLOAD_PATH = "/v2/upload";
export type AssemblyAITranscriptionOptions = {
    readonly keyterms_prompt?: ReadonlyArray<string>;
    readonly punctuate?: boolean;
    readonly format_text?: boolean;
    readonly disfluencies?: boolean;
    readonly filter_profanity?: boolean;
    readonly temperature?: number;
    readonly speaker_options?: {
        readonly min_speakers_expected?: number;
        readonly max_speakers_expected?: number;
    };
    readonly language_detection_options?: {
        readonly expected_languages?: ReadonlyArray<string>;
        readonly fallback_language?: string;
        readonly code_switching?: boolean;
    };
    readonly speech_models?: ReadonlyArray<OpenString<"universal-3-5-pro" | "universal-2">>;
} & Record<string, unknown>;
export type Request = TranscriptionRequestFor<AssemblyAITranscriptionOptions>;
export declare const Token: Schema.Struct<{
    readonly transcriptID: Schema.String;
}>;
export type Token = Schema.Schema.Type<typeof Token>;
export declare const protocol: MediaProtocol.Queued<Request, TranscriptionResponse, {
    readonly transcriptID: string;
}>;
export declare const model: (input: MediaRoute.ModelInput) => TranscriptionModel<AssemblyAITranscriptionOptions>;
export declare const AssemblyAITranscription: {
    readonly protocol: MediaProtocol.Queued<Request, TranscriptionResponse, {
        readonly transcriptID: string;
    }>;
    readonly model: typeof model;
};
