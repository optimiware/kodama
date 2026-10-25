import { Effect } from "effect";
import { Media } from "../../media.js";
import type { MediaProtocol } from "../../route/media-protocol.js";
import { type AIError, type ProviderID } from "../../schema/index.js";
/** Owned bytes for multipart uploads; decodes `base64` sources and rejects remote sources. */
export declare const inlineBytes: (route: string, asset: Media.Asset) => Effect.Effect<Uint8Array, AIError>;
/** Copied because `BlobPart` requires a plain `ArrayBuffer`. */
export declare const blob: (data: Uint8Array, mediaType: string) => Blob;
export declare const query: (route: string, values: Record<string, unknown>) => Effect.Effect<MediaProtocol.Query, AIError>;
export declare const dimensions: (size: string) => {
    width: number;
    height: number;
};
/** Provider file handle when the ref belongs to this provider; refs from other providers are never forwarded. */
export declare const refID: (asset: Media.Asset, provider: ProviderID) => string | undefined;
/** Decode a provider's base64 output once into an owned `bytes` asset, sniffing the type when it is not declared. */
export declare const decodedAsset: (invalid: (message: string, cause?: unknown) => AIError, label: string, data: string, mediaType: string | undefined, options?: Media.AssetOptions) => Effect.Effect<Media.Asset, AIError, never>;
/** One image of an OpenAI-shaped `data` array, which carries either `b64_json` or a `url`. */
export declare const imageOutput: (invalid: (message: string, cause?: unknown) => AIError, label: string, item: {
    readonly b64_json?: string | null;
    readonly url?: string | null;
}, mediaType: string | undefined, options?: Media.AssetOptions) => Effect.Effect<Media.Asset, AIError, never>;
/**
 * Append multipart text fields: strings as-is, other values as JSON, or scalar arrays as one part per item with
 * `repeatArrays`, named `key[]` or `key`. `overlay` keys in `reserved` are dropped so `http.body` cannot replace
 * route-owned fields.
 */
export declare const appendFields: (form: FormData, fields: Record<string, unknown>, options: {
    readonly overlay?: Record<string, unknown>;
    readonly reserved: ReadonlySet<string>;
    readonly repeatArrays?: "key[]" | "key";
}) => void;
export * as MediaInput from "./media-input.js";
