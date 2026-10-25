import { Schema } from "effect";
import { HttpApiEndpoint, HttpApiGroup } from "effect/http-api";
export declare const ReferenceGroup: HttpApiGroup.HttpApiGroup<"server.reference", HttpApiEndpoint.HttpApiEndpoint<"reference.list", "GET", "/api/reference", never, Schema.toCodecStringTree<Schema.Struct<{
    readonly location: Schema.optional<Schema.Struct<{
        readonly directory: Schema.optional<Schema.String>;
    }>>;
}>>, never, never, Schema.toCodecJson<Schema.Struct<{
    readonly location: Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    }>;
    readonly data: Schema.$Array<Schema.Struct<{
        readonly name: Schema.String;
        readonly path: Schema.brand<Schema.String, "AbsolutePath">;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        readonly source: Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"local">;
            readonly path: Schema.brand<Schema.String, "AbsolutePath">;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"git">;
            readonly repository: Schema.String;
            readonly branch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        }>]>;
    }>>;
}>>, never, never, never>, false>;
