export * as ConfigWorktree from "./worktree.js";
import { Schema } from "effect";
export declare const Info: Schema.Struct<{
    readonly directory: Schema.Trim;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
