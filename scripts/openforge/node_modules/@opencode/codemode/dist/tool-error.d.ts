import { Schema } from "effect";
declare const ToolError_base: Schema.Class<ToolError, Schema.TaggedStruct<"ToolError", {
    readonly message: Schema.String;
    readonly cause: Schema.optionalKey<Schema.Defect>;
}>, import("effect/Cause").YieldableError>;
/** Tool failure reported as `ToolFailure`. */
export declare class ToolError extends ToolError_base {
}
/** Creates a tool failure with an optional underlying cause. */
export declare const toolError: (message: string, cause?: unknown) => ToolError;
export {};
