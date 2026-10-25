/**
 * Low-level parser for HTTP `multipart/form-data` bodies.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type * as ByteSize from "../ByteSize.ts";
import type * as HeadersParser from "./MultipartParser/HeadersParser.ts";
/**
 * Metadata describing a multipart form part.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface PartInfo {
    readonly name: string;
    readonly filename?: string | undefined;
    readonly contentType: string;
    readonly contentTypeParameters: Record<string, string>;
    readonly contentDisposition: string;
    readonly contentDispositionParameters: Record<string, string>;
    readonly headers: Record<string, string | Array<string>>;
}
/**
 * An error produced while parsing a multipart body.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type MultipartError = {
    readonly _tag: "InvalidBoundary";
} | {
    readonly _tag: "BadHeaders";
    readonly error: HeadersParser.Failure;
} | {
    readonly _tag: "InvalidDisposition";
} | {
    readonly _tag: "ReachedLimit";
    readonly limit: "MaxParts" | "MaxTotalSize" | "MaxPartSize" | "MaxFieldSize";
} | {
    readonly _tag: "EndNotReached";
};
/**
 * Shared multipart parser configuration.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type BaseConfig = {
    readonly headers: Record<string, string>;
    readonly isFile?: ((info: PartInfo) => boolean) | undefined;
    readonly maxParts?: number | undefined;
    readonly maxTotalSize?: ByteSize.Input | undefined;
    readonly maxPartSize?: ByteSize.Input | undefined;
    readonly maxFieldSize?: ByteSize.Input | undefined;
};
/**
 * Multipart parser configuration with event callbacks.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Config = BaseConfig & {
    readonly onField: (info: PartInfo, value: Uint8Array) => void;
    readonly onFile: (info: PartInfo) => (chunk: Uint8Array | null) => void;
    readonly onError: (error: MultipartError) => void;
    readonly onDone: () => void;
};
/**
 * A streaming multipart parser.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Parser {
    readonly write: (chunk: Uint8Array) => void;
    readonly end: () => void;
}
/**
 * Creates a streaming multipart parser.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (options: Config) => Parser;
/**
 * Determines whether a multipart part should be treated as a file.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export declare const defaultIsFile: (info: PartInfo) => boolean;
/**
 * Decodes a multipart field using its declared character set.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export declare const decodeField: (info: PartInfo, value: Uint8Array) => string;
//# sourceMappingURL=MultipartParser.d.ts.map