import { ImageModel, ImageResponse, type ImageRequestFor } from "../image.js";
import { MediaProtocol } from "../route/media-protocol.js";
import { MediaRoute } from "../route/media.js";
import { type OpenString } from "../schema/index.js";
export declare const DEFAULT_BASE_URL = "https://api.meta.ai/v1";
/** Provider-native options. Common fields (`n`, `size`, `format`, `images`) live on the request. */
export type ImageOptions = {
    readonly responseFormat?: OpenString<"b64_json" | "url">;
    readonly reasoningStrength?: OpenString<"low" | "high">;
    readonly toolEnablement?: {
        readonly enable_image_search?: boolean;
        readonly enable_web_search?: boolean;
        readonly enable_shell?: boolean;
    };
    readonly [key: string]: unknown;
};
export type Request = ImageRequestFor<ImageOptions>;
export declare const protocol: MediaProtocol.Inline<Request, ImageResponse>;
export declare const model: (input: MediaRoute.ModelInput) => ImageModel<ImageOptions>;
export * as MetaImages from "./meta-images.js";
