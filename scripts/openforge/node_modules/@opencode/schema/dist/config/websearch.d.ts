export * as ConfigWebSearch from "./websearch.js";
import { Schema } from "effect";
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly provider: Schema.Union<readonly [Schema.Literal<"random">, Schema.brand<Schema.String, "WebSearch.ID">]>;
}>, {}>;
export declare class Info extends Info_base {
}
export declare const Selection: Schema.Union<readonly [Schema.Literal<false>, typeof Info]>;
export type Selection = typeof Selection.Type;
