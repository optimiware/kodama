export * as ConfigShell from "./shell.js";
import { Schema } from "effect";
export declare const Option: Schema.Struct<{
    readonly path: Schema.String;
    readonly name: Schema.String;
    readonly acceptable: Schema.Boolean;
}>;
export interface Option extends Schema.Schema.Type<typeof Option> {
}
