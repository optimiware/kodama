import { Schema } from "effect";
export declare const AssertInput: Schema.Struct<{
    readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
        create: () => string & import("effect/Brand").Brand<"SessionID">;
        descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
    };
    readonly agent: Schema.optional<Schema.brand<Schema.String, "Agent.ID">>;
    readonly action: Schema.String;
    readonly resources: Schema.$Array<Schema.String>;
    readonly save: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly source: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"tool">;
        readonly messageID: Schema.String;
        readonly id: Schema.String;
    }>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"tool">;
        readonly messageID: Schema.String;
        readonly id: Schema.String;
    }>]>>, never, never>;
}>;
export interface AssertInput extends Schema.Schema.Type<typeof AssertInput> {
}
