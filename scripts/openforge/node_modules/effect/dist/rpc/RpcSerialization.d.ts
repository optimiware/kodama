/**
 * Serializes RPC protocol messages for transports.
 *
 * `RpcSerialization` is the boundary between `RpcMessage` envelopes and the
 * bytes or strings carried by a transport. This module provides built-in
 * serializers for JSON, newline-delimited JSON, JSON-RPC 2.0, and SchemaBinary,
 * including framed formats that can decode multiple messages
 * from streaming chunks.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.ts";
import * as Layer from "../Layer.ts";
import * as Schema from "../Schema.ts";
/**
 * Builds the codec used to fill the `unknown` holes of RPC protocol messages,
 * such as request payloads, stream chunks, exits, and defects.
 *
 * **Details**
 *
 * The envelope around the hole is framed by {@link Parser}, which never sees the
 * schema. JSON based serializations return `Schema.toCodecJson`, so the encoded
 * hole is JSON. Other serializations may encode the hole as bytes; both fit the
 * `unknown` hole, so `RpcSerialization` has no type parameter.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export type CodecFor = <S extends Schema.Top>(schema: S) => Schema.Codec<S["Type"], unknown, S["DecodingServices"], S["EncodingServices"]>;
declare const RpcSerialization_base: Context.ServiceClass<RpcSerialization, "effect/rpc/RpcSerialization", {
    makeUnsafe(): Parser;
    readonly contentType: string;
    readonly includesFraming: boolean;
    readonly codecFor: CodecFor;
}>;
/**
 * Service that describes how RPC protocol messages are encoded and decoded,
 * including the content type and whether the serialization format provides
 * message framing.
 *
 * **When to use**
 *
 * Use to provide the serialization boundary shared by RPC clients and servers
 * for a chosen wire format.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class RpcSerialization extends RpcSerialization_base {
}
/**
 * A stateful parser for an RPC serialization format, able to decode input
 * chunks into protocol messages and encode messages for transport.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export interface Parser {
    readonly decode: (data: Uint8Array | string) => ReadonlyArray<unknown>;
    readonly encode: (response: unknown) => Uint8Array | string | undefined;
}
declare const MaxBufferSizeExceeded_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "MaxBufferSizeExceeded";
} & Readonly<A>;
/**
 * Error raised when a streaming parser retains more data than its configured
 * buffer limit.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class MaxBufferSizeExceeded extends MaxBufferSizeExceeded_base<{
    readonly maxBufferSize: number;
}> {
    get message(): string;
}
/**
 * Options shared by streaming RPC serialization formats.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export interface StreamOptions {
    /**
     * Maximum number of bytes or string code units retained for an incomplete frame.
     * The default is 16 MiB. Use `"unbounded"` to disable the limit.
     */
    readonly maxBufferSize?: number | "unbounded" | undefined;
}
/**
 * JSON RPC serialization for whole message payloads. It does not include
 * message framing, so it is intended for transports that frame responses
 * themselves.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export declare const json: RpcSerialization["Service"];
/**
 * Serializes RPC protocol messages as newline-delimited JSON, framing each message
 * with a trailing newline.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export declare const makeNdjson: (options?: StreamOptions) => RpcSerialization["Service"];
/**
 * Default newline-delimited JSON RPC serialization.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export declare const ndjson: RpcSerialization["Service"];
/**
 * Creates a JSON-RPC 2.0 serialization for RPC protocol messages without
 * additional message framing.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export declare const jsonRpc: (options?: {
    readonly contentType?: string | undefined;
}) => RpcSerialization["Service"];
/**
 * Creates a newline-delimited JSON-RPC 2.0 serialization for RPC protocol
 * messages.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export declare const ndJsonRpc: (options?: {
    readonly contentType?: string | undefined;
    readonly maxBufferSize?: number | "unbounded" | undefined;
}) => RpcSerialization["Service"];
/**
 * RPC serialization layer that uses JSON for serialization.
 *
 * **When to use**
 *
 * Use when you have a transport protocol that already provides message framing.
 *
 * @see {@link layerNdjson} for transports that need newline-delimited framing
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerJson: Layer.Layer<RpcSerialization>;
/**
 * RPC serialization layer that uses NDJSON for serialization.
 *
 * **When to use**
 *
 * Use when you have a transport protocol that does not provide message framing.
 *
 * @see {@link layerJson} for transports that already provide message framing
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerNdjson: Layer.Layer<RpcSerialization>;
/**
 * RPC serialization layer that uses NDJSON with custom streaming options.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerNdjsonWith: (options?: StreamOptions) => Layer.Layer<RpcSerialization>;
/**
 * RPC serialization layer that uses JSON-RPC for serialization.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerJsonRpc: (options?: {
    readonly contentType?: string | undefined;
}) => Layer.Layer<RpcSerialization>;
/**
 * RPC serialization layer that uses newline-delimited JSON-RPC for
 * serialization.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerNdJsonRpc: (options?: {
    readonly contentType?: string | undefined;
    readonly maxBufferSize?: number | "unbounded" | undefined;
}) => Layer.Layer<RpcSerialization>;
/**
 * RPC serialization layer that uses SchemaBinary with fingerprinted RPC
 * envelopes. Payload fingerprints are disabled by default to support compatible
 * schema evolution. Frames default to a 16 MiB maximum size. Use `"unbounded"`
 * to disable the frame-size limit.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerSchemaBinary: (options?: {
    readonly maxFrameSize?: number | "unbounded" | undefined;
    readonly fingerprintPayloads?: boolean | undefined;
}) => Layer.Layer<RpcSerialization>;
export {};
//# sourceMappingURL=RpcSerialization.d.ts.map