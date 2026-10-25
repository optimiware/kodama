import { MediaProtocol } from "../route/media-protocol.js";
import { MediaRoute } from "../route/media.js";
import { type OpenString } from "../schema/index.js";
import { VideoModel, VideoResponse, type VideoRequestFor } from "../video.js";
/**
 * Provider-native input. fal video endpoints are model-specific: `duration` is a string enum whose values differ per
 * model (`"8s"` for Veo, `"5"` for Kling), and last-frame fields are named per model (`end_image_url`,
 * `last_frame_url`, `tail_image_url`), so those pass through here instead of lowering from common fields.
 */
export type FalVideoOptions = {
    readonly duration?: OpenString<"4s" | "6s" | "8s" | "5" | "10">;
} & Record<string, unknown>;
export type Request = VideoRequestFor<FalVideoOptions>;
export declare const protocol: MediaProtocol.Queued<Request, VideoResponse, {
    readonly requestID: string;
    readonly statusURL: string;
    readonly responseURL: string;
    readonly cancelURL: string;
}>;
export declare const model: (input: MediaRoute.ModelInput) => VideoModel<FalVideoOptions>;
export declare const FalVideo: {
    readonly protocol: MediaProtocol.Queued<Request, VideoResponse, {
        readonly requestID: string;
        readonly statusURL: string;
        readonly responseURL: string;
        readonly cancelURL: string;
    }>;
    readonly model: typeof model;
};
