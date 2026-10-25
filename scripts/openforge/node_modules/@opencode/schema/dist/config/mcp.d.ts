export * as ConfigMCP from "./mcp.js";
import { Schema } from "effect";
import { Mcp } from "../mcp.js";
export declare const Timeout: typeof Mcp.TimeoutConfig;
export type Timeout = Mcp.TimeoutConfig;
export declare const Local: typeof Mcp.LocalConfig;
export type Local = Mcp.LocalConfig;
export declare const OAuth: typeof Mcp.OAuthConfig;
export type OAuth = Mcp.OAuthConfig;
export declare const Remote: typeof Mcp.RemoteConfig;
export type Remote = Mcp.RemoteConfig;
export declare const Server: Schema.toTaggedUnion<"type", readonly [typeof Mcp.LocalConfig, typeof Mcp.RemoteConfig]>;
export declare const Protocol: Schema.Literals<readonly ["legacy", "auto", "2026-07-28"]>;
export type Protocol = Mcp.Protocol;
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly timeout: Schema.decodeTo<Schema.optional<Schema.toType<typeof Mcp.TimeoutConfig>>, Schema.optionalKey<typeof Mcp.TimeoutConfig>, never, never>;
    readonly servers: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.toTaggedUnion<"type", readonly [typeof Mcp.LocalConfig, typeof Mcp.RemoteConfig]>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.toTaggedUnion<"type", readonly [typeof Mcp.LocalConfig, typeof Mcp.RemoteConfig]>>>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
