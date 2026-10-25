export * as Money from "./money.js";
import { Schema } from "effect";
export declare const USD: Schema.brand<Schema.Finite, "Money.USD"> & {
    zero: number & import("effect/Brand").Brand<"Money.USD">;
};
export type USD = typeof USD.Type;
export declare const USDPerMillionTokens: Schema.brand<Schema.Finite, "Money.USDPerMillionTokens"> & {
    zero: number & import("effect/Brand").Brand<"Money.USDPerMillionTokens">;
};
export type USDPerMillionTokens = typeof USDPerMillionTokens.Type;
