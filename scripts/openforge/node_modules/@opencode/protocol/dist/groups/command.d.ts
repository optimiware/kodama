import { Schema } from "effect";
import { HttpApiEndpoint, HttpApiGroup } from "effect/http-api";
export declare const CommandGroup: HttpApiGroup.HttpApiGroup<"server.command", HttpApiEndpoint.HttpApiEndpoint<"command.list", "GET", "/api/command", never, Schema.toCodecStringTree<Schema.Struct<{
    readonly location: Schema.optional<Schema.Struct<{
        readonly directory: Schema.optional<Schema.String>;
    }>>;
}>>, never, never, Schema.toCodecJson<Schema.Struct<{
    readonly location: Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    }>;
    readonly data: Schema.$Array<Schema.Struct<{
        readonly name: Schema.String;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>;
}>>, never, never, never>, false>;
