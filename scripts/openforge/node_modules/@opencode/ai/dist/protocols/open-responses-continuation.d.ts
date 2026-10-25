import type { WebSocketChannelDriver } from "../route/transport/index.js";
/**
 * Fields to send next to `previous_response_id` on an incremental step, or undefined to send the step in full.
 * Whether omitted fields carry over from the continued response is provider behavior the route must know.
 */
export type Shape = (request: Readonly<Record<string, unknown>>) => Readonly<Record<string, unknown>> | undefined;
export interface DriverInput {
    readonly id: string;
    readonly name: string;
    readonly request: Readonly<Record<string, unknown>>;
    readonly message: string;
    readonly base: WebSocketChannelDriver;
    readonly continuation?: Shape;
}
export declare const driver: (input: DriverInput) => WebSocketChannelDriver;
export * as OpenResponsesContinuation from "./open-responses-continuation.js";
