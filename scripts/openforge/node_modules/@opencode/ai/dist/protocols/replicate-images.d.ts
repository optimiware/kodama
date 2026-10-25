import { Schema } from "effect";
import { ImageModel, ImageResponse, type ImageRequestFor } from "../image.js";
import { MediaProtocol } from "../route/media-protocol.js";
import { MediaRoute } from "../route/media.js";
export declare const DEFAULT_BASE_URL = "https://api.replicate.com";
/** Model-defined `input` fields under the model's own names; `Media.Asset` values lower to URLs or data URLs. */
export type ReplicateImageOptions = Record<string, unknown>;
export type Request = ImageRequestFor<ReplicateImageOptions>;
export declare const Token: Schema.Struct<{
    readonly id: Schema.String;
    readonly getURL: Schema.String;
    readonly cancelURL: Schema.String;
}>;
export type Token = Schema.Schema.Type<typeof Token>;
export declare const protocol: MediaProtocol.Queued<Request, ImageResponse, {
    readonly id: string;
    readonly getURL: string;
    readonly cancelURL: string;
}>;
export declare const model: (input: MediaRoute.ModelInput) => ImageModel<ReplicateImageOptions>;
export declare const ReplicateImages: {
    readonly protocol: MediaProtocol.Queued<Request, ImageResponse, {
        readonly id: string;
        readonly getURL: string;
        readonly cancelURL: string;
    }>;
    readonly model: typeof model;
};
