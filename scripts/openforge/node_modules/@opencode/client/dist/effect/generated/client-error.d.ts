import { Schema } from "effect";
declare const ClientError_base: Schema.Class<ClientError, Schema.TaggedStruct<"ClientError", {
    readonly cause: Schema.Defect;
}>, import("effect/Cause").YieldableError>;
export declare class ClientError extends ClientError_base {
}
export {};
