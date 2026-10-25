export * as ConfigModel from "./model.js";
import { Schema } from "effect";
declare const Explicit: Schema.Struct<{
    readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
    readonly model: Schema.brand<Schema.String, "Model.ID">;
    readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
}>;
export interface Selection extends Schema.Schema.Type<typeof Explicit> {
}
export declare const Selection: Schema.decodeTo<Schema.Struct<{
    readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
    readonly model: Schema.brand<Schema.String, "Model.ID">;
    readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
}>, Schema.Union<readonly [Schema.String, Schema.Struct<{
    readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
    readonly model: Schema.brand<Schema.String, "Model.ID">;
    readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
}>]>, never, never>;
