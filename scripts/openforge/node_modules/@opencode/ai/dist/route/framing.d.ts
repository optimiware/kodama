import { Stream } from "effect";
import { AIError } from "../schema/index.js";
/**
 * Decode a streaming HTTP response body into provider-protocol frames.
 *
 * `Framing` is the byte-stream-shaped seam between transport and protocol:
 *
 * - SSE (`Framing.sse`) — UTF-8 decode the body, run the SSE channel decoder,
 *   and emit the `data:` payload of each non-empty event. The default drops
 *   `[DONE]`; protocols that use it as a terminal select `sseWithDone`.
 * - AWS event stream — length-prefixed binary frames with CRC checksums.
 *   Each emitted frame is one parsed binary event record.
 * - Media streams — newline-delimited JSON (`lines`) or the whole body as one
 *   frame (`document`); chunked binary bodies need no framing.
 *
 * The frame type is opaque to this layer; the protocol's event schema decodes
 * each frame before its state machine handles it.
 */
export interface Definition<Frame> {
    readonly id: string;
    readonly frame: (bytes: Stream.Stream<Uint8Array, AIError>) => Stream.Stream<Frame, AIError>;
    /** Original wire representation when framing transforms the provider payload. */
    readonly body?: (frame: Frame) => string | undefined;
}
/**
 * `framing` step for Server-Sent Events. Decodes UTF-8, runs the SSE channel
 * decoder, optionally filters named events, and drops empty events and known
 * keepalives that proxies send as data. `[DONE]` is dropped by default or
 * retained for protocols that use it as their stream boundary. Retry control events are ignored without
 * interrupting the stream. Decoder failures become provider output errors so
 * the public error channel stays `AIError`.
 */
export declare const sseFraming: (bytes: Stream.Stream<Uint8Array, AIError>, events?: ReadonlySet<string>, includeDone?: boolean) => Stream.Stream<string, AIError>;
/** Server-Sent Events framing. Used by every JSON-streaming HTTP provider. */
export declare const sse: Definition<string>;
/** Server-Sent Events framing that retains the conventional `[DONE]` sentinel. */
export declare const sseWithDone: Definition<string>;
/** SSE framing restricted to protocol-recognized event names. */
export declare const sseEvents: (events: ReadonlySet<string>) => Definition<string>;
export declare const lines: Definition<string>;
export declare const document: Definition<string>;
export * as Framing from "./framing.js";
