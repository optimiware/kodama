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
import * as Context from "../Context.js";
import * as Data from "../Data.js";
import * as SchemaBinary from "../encoding/SchemaBinary.js";
import * as Layer from "../Layer.js";
import * as Predicate from "../Predicate.js";
import { hasProperty } from "../Predicate.js";
import * as Schema from "../Schema.js";
import * as RpcMessage from "./RpcMessage.js";
const codecForJson = Schema.toCodecJson;
// shared by every parser that decodes whole frames, so creating a parser does
// not pay for a native TextDecoder
let sharedTextDecoder;
const decodeText = bytes => (sharedTextDecoder ??= new TextDecoder()).decode(bytes);
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
export class RpcSerialization extends /*#__PURE__*/Context.Service()("effect/rpc/RpcSerialization") {}
/**
 * Error raised when a streaming parser retains more data than its configured
 * buffer limit.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class MaxBufferSizeExceeded extends /*#__PURE__*/Data.TaggedError("MaxBufferSizeExceeded") {
  get message() {
    return `RPC serialization buffer exceeded the maximum size of ${this.maxBufferSize}`;
  }
}
const defaultMaxBufferSize = 16 * 1024 * 1024;
const isBufferSizeExceeded = (bufferSize, maxBufferSize) => maxBufferSize !== "unbounded" && bufferSize > maxBufferSize;
/**
 * JSON RPC serialization for whole message payloads. It does not include
 * message framing, so it is intended for transports that frame responses
 * themselves.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const json = /*#__PURE__*/RpcSerialization.of({
  contentType: "application/json",
  includesFraming: false,
  codecFor: codecForJson,
  makeUnsafe: () => ({
    decode: bytes => {
      const decoded = JSON.parse(typeof bytes === "string" ? bytes : decodeText(bytes));
      return Array.isArray(decoded) ? decoded : [decoded];
    },
    encode: response => JSON.stringify(response)
  })
});
/**
 * Serializes RPC protocol messages as newline-delimited JSON, framing each message
 * with a trailing newline.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const makeNdjson = options => {
  const maxBufferSize = options?.maxBufferSize ?? defaultMaxBufferSize;
  return RpcSerialization.of({
    contentType: "application/ndjson",
    includesFraming: true,
    codecFor: codecForJson,
    makeUnsafe: () => {
      // lazily created: string transports never need byte decoding
      let decoder;
      let buffer = "";
      const failMaxBufferSize = maxBufferSize => {
        buffer = "";
        throw new MaxBufferSizeExceeded({
          maxBufferSize
        });
      };
      return {
        decode: bytes => {
          buffer += typeof bytes === "string" ? bytes : (decoder ??= new TextDecoder()).decode(bytes, {
            stream: true
          });
          let position = 0;
          let nlIndex = buffer.indexOf("\n", position);
          const items = [];
          while (nlIndex !== -1) {
            if (isBufferSizeExceeded(nlIndex - position, maxBufferSize)) {
              failMaxBufferSize(maxBufferSize);
            }
            const line = buffer.slice(position, nlIndex);
            position = nlIndex + 1;
            try {
              items.push(JSON.parse(line));
            } catch {}
            nlIndex = buffer.indexOf("\n", position);
          }
          buffer = buffer.slice(position);
          if (isBufferSizeExceeded(buffer.length, maxBufferSize)) {
            failMaxBufferSize(maxBufferSize);
          }
          return items;
        },
        encode: response => {
          if (Array.isArray(response)) {
            if (response.length === 0) return undefined;
            let data = "";
            for (let i = 0; i < response.length; i++) {
              data += JSON.stringify(response[i]) + "\n";
            }
            return data;
          }
          return JSON.stringify(response) + "\n";
        }
      };
    }
  });
};
/**
 * Default newline-delimited JSON RPC serialization.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const ndjson = /*#__PURE__*/makeNdjson();
/**
 * Creates a JSON-RPC 2.0 serialization for RPC protocol messages without
 * additional message framing.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const jsonRpc = options => RpcSerialization.of({
  contentType: options?.contentType ?? "application/json",
  includesFraming: false,
  codecFor: codecForJson,
  makeUnsafe: () => {
    const batches = new Map();
    return {
      decode: bytes => {
        const decoded = JSON.parse(typeof bytes === "string" ? bytes : decodeText(bytes));
        return decodeJsonRpcRaw(decoded, batches);
      },
      encode: response => {
        const encoded = encodeJsonRpcResponse(response, batches);
        return encoded && JSON.stringify(encoded);
      }
    };
  }
});
/**
 * Creates a newline-delimited JSON-RPC 2.0 serialization for RPC protocol
 * messages.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const ndJsonRpc = options => RpcSerialization.of({
  contentType: options?.contentType ?? "application/json-rpc",
  includesFraming: true,
  codecFor: codecForJson,
  makeUnsafe: () => {
    const parser = makeNdjson({
      maxBufferSize: options?.maxBufferSize
    }).makeUnsafe();
    const batches = new Map();
    return {
      decode: bytes => {
        const frames = parser.decode(bytes);
        if (frames.length === 0) return [];
        const messages = [];
        for (let i = 0; i < frames.length; i++) {
          messages.push(...decodeJsonRpcRaw(frames[i], batches));
        }
        return messages;
      },
      encode: response => {
        const encoded = encodeJsonRpcResponse(response, batches);
        return encoded && parser.encode(encoded);
      }
    };
  }
});
function decodeJsonRpcRaw(decoded, batches) {
  if (Array.isArray(decoded)) {
    const batch = {
      size: 0,
      responses: new Map()
    };
    const messages = [];
    for (let i = 0; i < decoded.length; i++) {
      const item = decoded[i];
      if (!Predicate.isObject(item)) continue;
      const message = decodeJsonRpcMessage(item);
      messages.push(message);
      if (message._tag === "Request" && !message.isNotification) {
        batch.size++;
        batches.set(message.id, batch);
      }
    }
    return messages;
  }
  return Predicate.isObject(decoded) ? [decodeJsonRpcMessage(decoded)] : [];
}
function decodeJsonRpcMessage(decoded) {
  if (Object.hasOwn(decoded, "method")) {
    const request = decoded;
    if (Predicate.isNullish(request.id) && Predicate.isString(request.method) && request.method.startsWith("@effect/rpc/")) {
      const tag = request.method.slice("@effect/rpc/".length);
      const requestId = request.params?.requestId;
      return requestId !== undefined ? {
        _tag: tag,
        requestId
      } : {
        _tag: tag
      };
    }
    return {
      _tag: "Request",
      id: request.id ?? "",
      tag: request.method,
      payload: request.params ?? null,
      headers: request.headers ?? [],
      ...(Predicate.hasProperty(request, "id") ? {} : {
        isNotification: true
      }),
      ...(request.spanId ? {
        traceId: request.traceId,
        spanId: request.spanId,
        sampled: request.sampled
      } : {})
    };
  }
  const response = decoded;
  const hasError = Object.hasOwn(response, "error");
  if (hasError && response.error && response.error._tag === "Defect") {
    return {
      _tag: "Defect",
      defect: response.error.data
    };
  } else if (Object.hasOwn(response, "chunk") && response.chunk === true) {
    return {
      _tag: "Chunk",
      requestId: response.id ?? "",
      values: response.result
    };
  }
  return {
    _tag: "Exit",
    requestId: response.id ?? "",
    exit: hasError && response.error != null ? {
      _tag: "Failure",
      cause: response.error._tag === "Cause" ? response.error.data : [{
        _tag: "Fail",
        error: response.error
      }]
    } : {
      _tag: "Success",
      value: response.result
    }
  };
}
function encodeJsonRpcRaw(response, batches) {
  if (!("requestId" in response)) {
    return encodeJsonRpcMessage(response);
  }
  const batch = batches.get(response.requestId);
  if (batch) {
    batches.delete(response.requestId);
    batch.responses.set(response.requestId, response);
    if (batch.size === batch.responses.size) {
      return Array.from(batch.responses.values(), encodeJsonRpcMessage);
    }
    return undefined;
  }
  return encodeJsonRpcMessage(response);
}
function encodeJsonRpcResponse(response, batches) {
  if (Array.isArray(response) === false) {
    return encodeJsonRpcRaw(response, batches);
  }
  if (response.length === 0) {
    return undefined;
  }
  const encoded = [];
  for (let i = 0; i < response.length; i++) {
    const current = encodeJsonRpcRaw(response[i], batches);
    if (current !== undefined) {
      encoded.push(current);
    }
  }
  if (encoded.length === 0) {
    return undefined;
  }
  if (encoded.length === 1) {
    return encoded[0];
  }
  const messages = [];
  for (let i = 0; i < encoded.length; i++) {
    const current = encoded[i];
    if (Array.isArray(current)) {
      messages.push(...current);
    } else {
      messages.push(current);
    }
  }
  return messages;
}
function encodeJsonRpcMessage(response) {
  switch (response._tag) {
    case "Request":
      return {
        jsonrpc: "2.0",
        method: response.tag,
        params: response.payload,
        // a JSON-RPC notification is a request without an id
        ...(response.isNotification ? {} : {
          id: response.id
        }),
        ...(response.headers?.length > 0 ? {
          headers: response.headers
        } : {}),
        traceId: response.traceId,
        spanId: response.spanId,
        sampled: response.sampled
      };
    case "Ping":
    case "Pong":
    case "Interrupt":
    case "Ack":
    case "Eof":
      return {
        jsonrpc: "2.0",
        method: `@effect/rpc/${response._tag}`,
        params: "requestId" in response ? {
          requestId: response.requestId
        } : undefined
      };
    case "Chunk":
      return {
        jsonrpc: "2.0",
        chunk: true,
        id: response.requestId,
        result: response.values
      };
    case "Exit":
      {
        if (response.exit._tag === "Success") {
          return {
            jsonrpc: "2.0",
            id: response.requestId ?? undefined,
            result: response.exit.value
          };
        }
        const failure = response.exit.cause.find(failure => failure._tag === "Fail");
        const error = failure?._tag === "Fail" ? failure.error : undefined;
        return {
          jsonrpc: "2.0",
          id: response.requestId ?? undefined,
          error: response.exit._tag === "Failure" ? {
            _tag: "Cause",
            code: error && Predicate.hasProperty(error, "code") ? Number(error.code) : 0,
            message: error && hasProperty(error, "message") ? error.message : JSON.stringify(response.exit.cause),
            data: response.exit.cause
          } : undefined
        };
      }
    case "Defect":
      return {
        jsonrpc: "2.0",
        id: jsonRpcInternalError,
        error: {
          _tag: "Defect",
          code: 1,
          message: "A defect occurred",
          data: response.defect
        }
      };
    case "ClientProtocolError":
      return {};
  }
}
const jsonRpcInternalError = -32603;
const defaultSchemaBinaryMaxFrameSize = 16 * 1024 * 1024;
const schemaBinaryTextEncoder = /*#__PURE__*/new TextEncoder();
const makeSchemaBinary = options => {
  const maxFrameSize = options?.maxFrameSize === "unbounded" ? undefined : options?.maxFrameSize ?? defaultSchemaBinaryMaxFrameSize;
  const codecFor = options?.fingerprintPayloads === true ? schema => SchemaBinary.toCodecDirect(schema, {
    fingerprint: true
  }) : SchemaBinary.toCodecDirect;
  const envelopeOptions = {
    fingerprint: true
  };
  return RpcSerialization.of({
    contentType: "application/vnd.effect.rpc+schema-binary",
    includesFraming: true,
    codecFor,
    makeUnsafe: () => {
      const parser = SchemaBinary.parser(RpcMessage.EncodedSchema, {
        ...envelopeOptions,
        maxFrameSize
      });
      const encoder = SchemaBinary.encoder(RpcMessage.EncodedSchema, envelopeOptions);
      return {
        decode: data => parser.feedSync(typeof data === "string" ? schemaBinaryTextEncoder.encode(data) : data),
        encode: response => {
          if (!Array.isArray(response)) {
            return encoder.encode(response);
          }
          if (response.length === 0) return undefined;
          return encoder.encodeMany(response);
        }
      };
    }
  });
};
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
export const layerJson = /*#__PURE__*/Layer.succeed(RpcSerialization)(json);
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
export const layerNdjson = /*#__PURE__*/Layer.succeed(RpcSerialization)(ndjson);
/**
 * RPC serialization layer that uses NDJSON with custom streaming options.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerNdjsonWith = options => Layer.succeed(RpcSerialization)(makeNdjson(options));
/**
 * RPC serialization layer that uses JSON-RPC for serialization.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerJsonRpc = options => Layer.succeed(RpcSerialization)(jsonRpc(options));
/**
 * RPC serialization layer that uses newline-delimited JSON-RPC for
 * serialization.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerNdJsonRpc = options => Layer.succeed(RpcSerialization)(ndJsonRpc(options));
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
export const layerSchemaBinary = options => Layer.sync(RpcSerialization)(() => makeSchemaBinary(options));
//# sourceMappingURL=RpcSerialization.js.map