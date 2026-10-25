export * as TokenUsage from "./token-usage.js";
import { Schema } from "effect";
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const Info: Schema.Struct<{
    readonly input: Schema.Finite;
    readonly output: Schema.Finite;
    readonly reasoning: Schema.Finite;
    readonly cache: Schema.Struct<{
        readonly read: Schema.Finite;
        readonly write: Schema.Finite;
    }>;
}>;
export declare function total(tokens: Info): number;
