import { Effect, Stream } from "effect";
import { Auth } from "./auth.js";
import { Endpoint } from "./endpoint.js";
import { type Interface } from "./executor-service.js";
import { MediaProtocol } from "./media-protocol.js";
import { Generation } from "../generation.js";
import { AIError, HttpOptions, ProviderID } from "../schema/index.js";
export type Execute = Interface["execute"];
/** The minimum a media request must carry for the route to build a transport request. */
export interface MediaRequest {
    readonly model: {
        readonly id: string;
        readonly provider: ProviderID;
        readonly http?: HttpOptions;
    };
    readonly http?: HttpOptions;
}
/** Deployment inputs every media model factory accepts; provider facades fill these from `configure(...)`. */
export interface ModelInput {
    readonly id: string;
    readonly auth: Auth.Definition;
    readonly baseURL?: string;
    readonly headers?: Record<string, string>;
    readonly http?: HttpOptions;
}
/** A provider facade's `configure(...)` input as the `ModelInput` every media selector shares, minus the model id. */
export declare const deployment: (input: {
    readonly baseURL?: string;
    readonly headers?: Record<string, string>;
    readonly http?: HttpOptions.Input;
}, auth: Auth.Definition) => Omit<ModelInput, "id">;
/** One request, one response. */
export interface InlineRoute<Request extends MediaRequest, Response> {
    readonly kind: "inline";
    readonly id: string;
    readonly provider: ProviderID;
    readonly protocol: string;
    readonly generate: (request: Request, execute: Execute) => Effect.Effect<Response, AIError>;
}
/** Submit, then poll through the returned `Generation`. */
export interface QueuedRoute<Request extends MediaRequest, Response> {
    readonly kind: "queued";
    readonly id: string;
    readonly provider: ProviderID;
    readonly protocol: string;
    readonly start: (request: Request, execute: Execute) => Effect.Effect<Generation<Response>, AIError>;
    /** Rebuild a handle from a persisted `Generation.token`; fails typed when the token is not this route's. */
    readonly resume: (model: MediaRequest["model"], token: unknown, execute: Execute) => Effect.Effect<Generation<Response>, AIError>;
}
/** One request whose response parses into events; `generate` runs the same stream and collects it. */
export interface StreamRoute<Request extends MediaRequest, Event, Response> {
    readonly kind: "stream";
    readonly id: string;
    readonly provider: ProviderID;
    readonly protocol: string;
    readonly stream: (request: Request, execute: Execute) => Stream.Stream<Event, AIError>;
    readonly generate: (request: Request, execute: Execute) => Effect.Effect<Response, AIError>;
}
export type AnyRoute<Request extends MediaRequest, Event, Response> = InlineRoute<Request, Response> | StreamRoute<Request, Event, Response> | QueuedRoute<Request, Response>;
export interface Composition<Request extends MediaRequest> {
    readonly endpoint: Endpoint.Definition<MediaProtocol.Body, Request>;
    readonly auth: Auth.Definition;
    /** Deployment headers applied before transport authentication. */
    readonly headers?: Record<string, string>;
}
export interface InlineInput<Request extends MediaRequest, Response> extends Composition<Request> {
    readonly protocol: MediaProtocol.Inline<Request, Response>;
}
export interface QueuedInput<Request extends MediaRequest, Response, Token> extends Composition<Request> {
    readonly protocol: MediaProtocol.Queued<Request, Response, Token>;
}
export interface StreamInput<Request extends MediaRequest, Event, Response, Frame, State> extends Composition<MediaProtocol.Addressed<Request>> {
    readonly protocol: MediaProtocol.Streamed<Request, Event, Frame, State>;
    readonly collect: (events: ReadonlyArray<Event>) => Effect.Effect<Response, AIError>;
}
/**
 * Compose an inline media protocol with an endpoint and auth into a runnable route. The route owns the transport
 * plumbing every media protocol would otherwise duplicate: option merging, surrogate sanitizing, unsupported-field
 * rejection, URL and query rendering, auth headers, JSON, multipart, or binary encoding, and handing responses back
 * to the protocol.
 */
export declare const inline: <Request extends MediaRequest, Response>(input: InlineInput<Request, Response>) => InlineRoute<Request, Response>;
/**
 * Compose a queued media protocol the same way, adding `start`/`resume` handles whose polls reuse the route's auth,
 * deployment headers, and (for `start`) the request's `http` overlay. The token is decoded once at the boundary and
 * closed over by the resulting `Generation.Route`.
 */
export declare const queued: <Request extends MediaRequest, Response, Token>(input: QueuedInput<Request, Response, Token>) => QueuedRoute<Request, Response>;
/** Compose a streaming media protocol; `generate` runs the same stream in `generate` mode and folds it with `collect`. */
export declare const stream: <Request extends MediaRequest, Event, Response, Frame, State>(input: StreamInput<Request, Event, Response, Frame, State>) => StreamRoute<Request, Event, Response>;
export * as MediaRoute from "./media.js";
