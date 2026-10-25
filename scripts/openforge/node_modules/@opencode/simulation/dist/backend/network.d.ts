import { Effect, Layer } from "effect";
import { HttpClient, HttpClientResponse, type HttpMethod } from "effect/http";
import { HttpClientError } from "effect/http/HttpClientError";
import type { HttpClientRequest } from "effect/http";
import { SimulationProtocol } from "../protocol";
/**
 * Simulated network.
 *
 * Replaces the `HttpClient.HttpClient` platform node in simulation mode. All
 * outbound HTTP resolves against an in-memory route table; unknown
 * destinations fail loudly with a transport error so no simulation run can
 * silently reach the real network. The scripted LLM is one registered route,
 * not a separate mechanism.
 *
 * Each acquired run owns its routes and request log.
 */
export interface Route {
    /** Return a response effect to claim the request, undefined to pass. */
    readonly match: (request: HttpClientRequest.HttpClientRequest, url: URL) => Effect.Effect<HttpClientResponse.HttpClientResponse, HttpClientError> | undefined;
}
export type LogEntry = SimulationProtocol.Backend.NetworkLogEntry;
/** Static JSON route: exact method + origin/path match answered with a fixed body. */
export declare function json(method: HttpMethod.HttpMethod, url: string, body: unknown): Route;
export interface Run {
    readonly client: HttpClient.HttpClient;
    readonly log: () => Effect.Effect<readonly LogEntry[]>;
}
export declare const make: (routes?: readonly Route[] | undefined) => Effect.Effect<{
    client: HttpClient.HttpClient;
    log: () => Effect.Effect<readonly SimulationProtocol.Backend.NetworkLogEntry[], never, never>;
}, never, never>;
export declare const layer: (routes?: readonly Route[]) => Layer.Layer<HttpClient.HttpClient, never, never>;
export * as SimulationNetwork from "./network";
