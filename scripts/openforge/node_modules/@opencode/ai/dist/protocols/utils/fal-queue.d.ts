import { Effect, Schema } from "effect";
import type { HttpClientResponse } from "effect/http";
import type { Media } from "../../media.js";
import { MediaProtocol } from "../../route/media-protocol.js";
import type { AIError } from "../../schema/index.js";
export declare const DEFAULT_BASE_URL = "https://queue.fal.run";
/** fal hands back absolute follow-up URLs on submit; they are authoritative for status, result, and cancel. */
export declare const Token: Schema.Struct<{
    readonly requestID: Schema.String;
    readonly statusURL: Schema.String;
    readonly responseURL: Schema.String;
    readonly cancelURL: Schema.String;
}>;
export type Token = Schema.Schema.Type<typeof Token>;
export declare const mediaUrl: (asset: Media.Asset, name: string) => Effect.Effect<string, AIError, never>;
export declare const protocol: <Request, Response>(route: MediaProtocol.Identity, input: {
    readonly unsupported?: ReadonlyArray<keyof Request & string>;
    readonly from: (request: Request) => Effect.Effect<MediaProtocol.Body, AIError>;
    readonly decodeResult: (response: HttpClientResponse.HttpClientResponse, context: MediaProtocol.PollContext<Token>) => Effect.Effect<Response, AIError>;
}) => MediaProtocol.Queued<Request, Response, {
    readonly requestID: string;
    readonly statusURL: string;
    readonly responseURL: string;
    readonly cancelURL: string;
}>;
export * as FalQueue from "./fal-queue.js";
