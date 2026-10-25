export * as Mcp from "./mcp.js";
import { Schema } from "effect";
declare const TimeoutConfig_base: Schema.Class<TimeoutConfig, Schema.Struct<{
    readonly startup: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly catalog: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly execution: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>, {}>;
export declare class TimeoutConfig extends TimeoutConfig_base {
}
export type Protocol = typeof Protocol.Type;
export declare const Protocol: Schema.Literals<readonly ["legacy", "auto", "2026-07-28"]>;
declare const LocalConfig_base: Schema.Class<LocalConfig, Schema.Struct<{
    readonly type: Schema.Literal<"local">;
    readonly command: Schema.$Array<Schema.String>;
    readonly cwd: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly environment: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
    readonly disabled: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly codemode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly timeout: Schema.decodeTo<Schema.optional<Schema.toType<typeof TimeoutConfig>>, Schema.optionalKey<typeof TimeoutConfig>, never, never>;
    readonly protocol: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["legacy", "auto", "2026-07-28"]>>>, Schema.optionalKey<Schema.Literals<readonly ["legacy", "auto", "2026-07-28"]>>, never, never>;
}>, {}>;
export declare class LocalConfig extends LocalConfig_base {
}
declare const OAuthConfig_base: Schema.Class<OAuthConfig, Schema.Struct<{
    readonly client_id: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly client_secret: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly scope: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly callback_port: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly redirect_uri: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly auth_server_metadata_url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}>;
export declare class OAuthConfig extends OAuthConfig_base {
}
declare const RemoteConfig_base: Schema.Class<RemoteConfig, Schema.Struct<{
    readonly type: Schema.Literal<"remote">;
    readonly url: Schema.String;
    readonly headers: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
    readonly oauth: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [typeof OAuthConfig, Schema.Literal<false>]>>>, Schema.optionalKey<Schema.Union<readonly [typeof OAuthConfig, Schema.Literal<false>]>>, never, never>;
    readonly disabled: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly codemode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly timeout: Schema.decodeTo<Schema.optional<Schema.toType<typeof TimeoutConfig>>, Schema.optionalKey<typeof TimeoutConfig>, never, never>;
    readonly protocol: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["legacy", "auto", "2026-07-28"]>>>, Schema.optionalKey<Schema.Literals<readonly ["legacy", "auto", "2026-07-28"]>>, never, never>;
}>, {}>;
export declare class RemoteConfig extends RemoteConfig_base {
}
export declare const ServerConfig: Schema.toTaggedUnion<"type", readonly [typeof LocalConfig, typeof RemoteConfig]>;
export type ServerConfig = typeof ServerConfig.Type;
export type Status = typeof Status.Type;
export declare const Status: Schema.toTaggedUnion<"status", readonly [Schema.Struct<{
    readonly status: Schema.Literal<"connected">;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"pending">;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"disabled">;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"failed">;
    readonly error: Schema.String;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"needs_auth">;
    readonly error: Schema.String;
}>]>;
export interface Server extends Schema.Schema.Type<typeof Server> {
}
export declare const Server: Schema.Struct<{
    readonly name: Schema.String;
    readonly status: Schema.toTaggedUnion<"status", readonly [Schema.Struct<{
        readonly status: Schema.Literal<"connected">;
    }>, Schema.Struct<{
        readonly status: Schema.Literal<"pending">;
    }>, Schema.Struct<{
        readonly status: Schema.Literal<"disabled">;
    }>, Schema.Struct<{
        readonly status: Schema.Literal<"failed">;
        readonly error: Schema.String;
    }>, Schema.Struct<{
        readonly status: Schema.Literal<"needs_auth">;
        readonly error: Schema.String;
    }>]>;
    readonly integrationID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Integration.ID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Integration.ID">>, never, never>;
}>;
export interface Resource extends Schema.Schema.Type<typeof Resource> {
}
export declare const Resource: Schema.Struct<{
    readonly server: Schema.String;
    readonly name: Schema.String;
    readonly uri: Schema.String;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface ResourceTemplate extends Schema.Schema.Type<typeof ResourceTemplate> {
}
export declare const ResourceTemplate: Schema.Struct<{
    readonly server: Schema.String;
    readonly name: Schema.String;
    readonly uriTemplate: Schema.String;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface ResourceCatalog extends Schema.Schema.Type<typeof ResourceCatalog> {
}
export declare const ResourceCatalog: Schema.Struct<{
    readonly resources: Schema.$Array<Schema.Struct<{
        readonly server: Schema.String;
        readonly name: Schema.String;
        readonly uri: Schema.String;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>;
    readonly templates: Schema.$Array<Schema.Struct<{
        readonly server: Schema.String;
        readonly name: Schema.String;
        readonly uriTemplate: Schema.String;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>;
}>;
export declare const ResourceContentPart: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"text">;
    readonly uri: Schema.String;
    readonly text: Schema.String;
    readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"blob">;
    readonly uri: Schema.String;
    readonly blob: Schema.String;
    readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>]>;
export type ResourceContentPart = typeof ResourceContentPart.Type;
export interface ResourceContent extends Schema.Schema.Type<typeof ResourceContent> {
}
export declare const ResourceContent: Schema.Struct<{
    readonly server: Schema.String;
    readonly uri: Schema.String;
    readonly contents: Schema.$Array<Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"text">;
        readonly uri: Schema.String;
        readonly text: Schema.String;
        readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"blob">;
        readonly uri: Schema.String;
        readonly blob: Schema.String;
        readonly mimeType: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>]>>;
}>;
