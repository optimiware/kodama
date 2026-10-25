export * as ConfigLSP from "./lsp.js";
import { Schema } from "effect";
export declare const Disabled: Schema.Struct<{
    readonly disabled: Schema.Literal<true>;
}>;
declare const Server_base: Schema.Class<Server, Schema.Struct<{
    readonly command: Schema.$Array<Schema.String>;
    readonly extensions: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly disabled: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly env: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
    readonly initialization: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
}>, {}>;
export declare class Server extends Server_base {
}
export declare const Entry: Schema.Union<readonly [Schema.Struct<{
    readonly disabled: Schema.Literal<true>;
}>, typeof Server]>;
export declare const Info: Schema.Union<readonly [Schema.Boolean, Schema.$Record<Schema.String, Schema.Union<readonly [Schema.Struct<{
    readonly disabled: Schema.Literal<true>;
}>, typeof Server]>>]>;
