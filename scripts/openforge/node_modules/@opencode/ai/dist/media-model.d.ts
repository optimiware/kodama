import { Effect } from "effect";
import { Endpoint } from "./route/endpoint.js";
import { MediaRoute } from "./route/media.js";
import type { MediaProtocol } from "./route/media-protocol.js";
import { AIError, HttpOptions, ModelID, ProviderID } from "./schema/index.js";
/**
 * What every media model carries: ids, the configured route, and deployment `http` overlays. Modality classes
 * (`ImageModel`, `VideoModel`, `SpeechModel`, `TranscriptionModel`) extend it with their route type and a nominal
 * marker so one cannot stand in for the other in requests.
 */
export declare class MediaModel<Route, Options> {
    protected readonly _Options: Options;
    readonly id: ModelID;
    readonly provider: ProviderID;
    readonly route: Route;
    readonly http?: HttpOptions;
    constructor(input: MediaModel.Input<Route>);
}
export declare namespace MediaModel {
    type Options = Record<string, unknown>;
    interface Input<Route> {
        readonly id: string | ModelID;
        readonly provider: string | ProviderID;
        readonly route: Route;
        readonly http?: HttpOptions;
    }
    /** A protocol plus its canonical start path; `ModelInput.baseURL` overrides `baseURL` per deployment. */
    interface RouteInput<Request extends MediaRoute.MediaRequest, Protocol> {
        readonly protocol: Protocol;
        readonly path: Endpoint.EndpointPart<MediaProtocol.Body, Request>;
        readonly baseURL?: string;
        /** Headers the protocol requires on every call, such as a pinned API version; deployment headers win. */
        readonly headers?: Record<string, string>;
    }
    type InlineRouteInput<Request extends MediaRoute.MediaRequest, Response> = RouteInput<Request, MediaProtocol.Inline<Request, Response>>;
    type StreamRouteInput<Request extends MediaRoute.MediaRequest, Event, Frame, State> = RouteInput<MediaProtocol.Addressed<Request>, MediaProtocol.Streamed<Request, Event, Frame, State>>;
    type QueuedRouteInput<Request extends MediaRoute.MediaRequest, Response, Token> = RouteInput<Request, MediaProtocol.Queued<Request, Response, Token>>;
    type AnyRouteInput<Request extends MediaRoute.MediaRequest, Event, Response, Frame, State, Token> = InlineRouteInput<Request, Response> | StreamRouteInput<Request, Event, Frame, State> | QueuedRouteInput<Request, Response, Token>;
}
/** Compose a protocol route input with one deployment through `MediaRoute.inline`, `queued`, or `stream`. */
export declare const composeRoute: <Request extends MediaRoute.MediaRequest, Event, Response, Frame, State, Token>(route: MediaModel.AnyRouteInput<Request, Event, Response, Frame, State, Token>, input: MediaRoute.ModelInput, collect: (events: ReadonlyArray<Event>) => Effect.Effect<Response, AIError>) => MediaRoute.AnyRoute<Request, Event, Response>;
/** Lift a synchronous Schema-class constructor into a typed `InvalidRequest` failure. */
export declare const tryRequest: <A>(make: () => A) => Effect.Effect<A, AIError>;
