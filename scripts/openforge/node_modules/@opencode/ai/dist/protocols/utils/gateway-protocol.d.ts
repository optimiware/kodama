import { Effect } from "effect";
import { Protocol } from "../../route/protocol.js";
import { type AIError, type LLMRequest } from "../../schema/index.js";
interface ParserState<Inner> {
    readonly inner: Inner;
    readonly gateway?: Record<string, unknown>;
}
export declare function gatewayProtocol<Body, Event, State>(protocol: Protocol<Body, string, Event, State>, input: {
    readonly id: string;
    readonly prepare: (request: LLMRequest) => Effect.Effect<{
        readonly request: LLMRequest;
        readonly body: Record<string, unknown>;
    }, AIError>;
}): Protocol<{
    readonly [x: string]: unknown;
}, string, Event, ParserState<State>>;
export {};
