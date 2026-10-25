export * as ConfigCommand from "./command.js";
import { Schema } from "effect";
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly template: Schema.String;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly agent: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly model: Schema.decodeTo<Schema.optional<Schema.toType<Schema.decodeTo<Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>, Schema.Union<readonly [Schema.String, Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>]>, never, never>>>, Schema.optionalKey<Schema.decodeTo<Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>, Schema.Union<readonly [Schema.String, Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>]>, never, never>>, never, never>;
    readonly subagent: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly subtask: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
