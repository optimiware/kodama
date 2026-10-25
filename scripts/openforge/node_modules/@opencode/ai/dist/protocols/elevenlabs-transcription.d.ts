import { MediaProtocol } from "../route/media-protocol.js";
import { MediaRoute } from "../route/media.js";
import { type OpenString } from "../schema/index.js";
import { TranscriptionModel, TranscriptionResponse, type TranscriptionRequestFor } from "../transcription.js";
export declare const DEFAULT_BASE_URL = "https://api.elevenlabs.io";
export declare const PATH = "/v1/speech-to-text";
export type ElevenLabsTranscriptionOptions = {
    readonly tag_audio_events?: boolean;
    readonly timestamps_granularity?: OpenString<"none" | "word" | "character">;
    readonly diarization_threshold?: number;
    readonly file_format?: OpenString<"pcm_s16le_16" | "other">;
    readonly temperature?: number;
    readonly seed?: number;
    readonly keyterms?: ReadonlyArray<string>;
    readonly no_verbatim?: boolean;
    readonly detect_speaker_roles?: boolean;
    readonly use_speaker_library?: boolean;
    readonly entity_detection?: string | ReadonlyArray<string>;
    readonly entity_redaction?: string | ReadonlyArray<string>;
    readonly entity_redaction_mode?: OpenString<"redacted" | "entity_type" | "enumerated_entity_type">;
} & Record<string, unknown>;
export type Request = TranscriptionRequestFor<ElevenLabsTranscriptionOptions>;
export declare const protocol: MediaProtocol.Inline<Request, TranscriptionResponse>;
export declare const model: (input: MediaRoute.ModelInput) => TranscriptionModel<ElevenLabsTranscriptionOptions>;
export declare const ElevenLabsTranscription: {
    readonly protocol: MediaProtocol.Inline<Request, TranscriptionResponse>;
    readonly model: typeof model;
};
