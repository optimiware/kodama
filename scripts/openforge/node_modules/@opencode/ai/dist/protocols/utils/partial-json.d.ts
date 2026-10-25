export * from "./partial-json-options.js";
export declare class PartialJSON extends Error {
}
export declare class MalformedJSON extends Error {
}
/** Parse complete or incomplete JSON, restricted by the supplied partial-value flags. */
export declare function parseJSON(jsonString: string, allowPartial?: number): unknown;
export declare const parse: typeof parseJSON;
