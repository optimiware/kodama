export * as ConfigAgent from "./agent.js";
import { Schema } from "effect";
import { ConfigProvider } from "./provider.js";
export declare const Color: Schema.String;
declare const Info_base: Schema.Class<Info, Schema.Struct<{
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
    readonly request: Schema.decodeTo<Schema.optional<Schema.toType<typeof ConfigProvider.Request>>, Schema.optionalKey<typeof ConfigProvider.Request>, never, never>;
    readonly system: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly mode: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["subagent", "primary", "all"]>>>, Schema.optionalKey<Schema.Literals<readonly ["subagent", "primary", "all"]>>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly steps: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly disabled: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly permissions: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
        readonly action: Schema.String;
        readonly resource: Schema.String;
        readonly effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
    }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
        readonly action: Schema.String;
        readonly resource: Schema.String;
        readonly effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
    }>>>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
