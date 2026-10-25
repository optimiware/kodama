import { Schema } from "effect";
/** Stable string identifier for a protocol implementation. */
export declare const ProtocolID: Schema.String;
export type ProtocolID = Schema.Schema.Type<typeof ProtocolID>;
/** Stable string identifier for the runnable route. */
export declare const RouteID: Schema.String;
export type RouteID = Schema.Schema.Type<typeof RouteID>;
export declare const ModelID: Schema.brand<Schema.String, "AI.ModelID">;
export type ModelID = typeof ModelID.Type;
export declare const ProviderID: Schema.brand<Schema.String, "AI.ProviderID">;
export type ProviderID = typeof ProviderID.Type;
export declare const ResponseID: Schema.String;
export type ResponseID = Schema.Schema.Type<typeof ResponseID>;
export declare const ContentBlockID: Schema.String;
export type ContentBlockID = Schema.Schema.Type<typeof ContentBlockID>;
export declare const ToolCallID: Schema.String;
export type ToolCallID = Schema.Schema.Type<typeof ToolCallID>;
