import { Schema } from "effect";
import { HttpApiEndpoint, HttpApiGroup } from "effect/http-api";
export declare const SkillGroup: HttpApiGroup.HttpApiGroup<"server.skill", HttpApiEndpoint.HttpApiEndpoint<"skill.list", "GET", "/api/skill", never, Schema.toCodecStringTree<Schema.Struct<{
    readonly location: Schema.optional<Schema.Struct<{
        readonly directory: Schema.optional<Schema.String>;
    }>>;
}>>, never, never, Schema.toCodecJson<Schema.Struct<{
    readonly location: Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    }>;
    readonly data: Schema.$Array<Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Skill.ID">;
        readonly name: Schema.brand<Schema.String, "Skill.Name">;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly autoinvoke: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        readonly path: Schema.brand<Schema.String, "AbsolutePath">;
        readonly content: Schema.String;
    }>>;
}>>, never, never, never>, false>;
