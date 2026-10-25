import { Framing } from "../route/framing.js";
/**
 * AWS event-stream framing for Bedrock Converse. Each frame is decoded by
 * `@smithy/eventstream-codec` (length + header + payload + CRC) and rewrapped
 * under its `:event-type` header so the chunk schema can match the JSON
 * payload directly.
 */
export declare const framing: (route: string) => Framing.Definition<object>;
export * as BedrockEventStream from "./bedrock-event-stream.js";
