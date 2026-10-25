import { Schema } from "effect";
export declare const Json: Schema.fromJsonString<Schema.Unknown>;
export declare const decodeJson: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => unknown;
export declare const encodeJson: (input: unknown, options?: import("effect/SchemaAST").ParseOptions) => string;
