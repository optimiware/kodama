import * as Effect from "../../Effect.js";
import { identity } from "../../Function.js";
import * as Stream from "../../Stream.js";
import * as HttpBody from "../HttpBody.js";
import * as Response from "../HttpServerResponse.js";
/**
 * @internal
 */
export const varyWith = (headers, dimension) => {
  const vary = headers["vary"];
  if (vary === undefined) {
    return dimension;
  }
  const members = vary.split(",").map(member => member.trim().toLowerCase());
  return members.includes("*") || members.includes(dimension.toLowerCase()) ? vary : `${vary}, ${dimension}`;
};
/**
 * @internal
 */
export const wrapCompression = impl => ({
  algorithms: impl.algorithms,
  compressResponse(response, algorithm, options) {
    return Effect.map(impl.compressResponse(response, algorithm, options), compressed => {
      if (compressed === response) {
        return response;
      }
      const headers = {
        "content-encoding": algorithm,
        vary: varyWith(compressed.headers, "Accept-Encoding")
      };
      const etag = compressed.headers["etag"];
      if (etag !== undefined && !etag.startsWith("W/")) {
        headers["etag"] = `W/${etag}`;
      }
      return Response.setHeaders(compressed, headers);
    });
  }
});
/**
 * @internal
 */
export const compressionTransformWeb = format => stream => stream.pipeThrough(new CompressionStream(format));
/**
 * @internal
 */
export const setBodyWithoutLength = (response, body) => Response.removeHeader(Response.setBody(response, body), "content-length");
/**
 * @internal
 */
export const makeCompressionWeb = options => ({
  algorithms: new Set(options.algorithms),
  compressResponse(response, algorithm, opts) {
    const body = response.body;
    switch (body._tag) {
      case "Uint8Array":
        {
          const data = body.body;
          return Effect.succeed(streamBody(response, () => options.transform(algorithm, opts)(singleChunkStream(data)), response.headers["content-type"] ?? body.contentType));
        }
      case "Stream":
        {
          const stream = body.stream;
          return Effect.succeed(streamBody(response, () => options.transform(algorithm, opts)(Stream.toReadableStream(stream)), response.headers["content-type"] ?? body.contentType));
        }
      case "Raw":
        {
          const readable = rawReadableStream(body.body);
          if (readable === undefined) {
            return Effect.succeed(response);
          }
          return Effect.succeed(setBodyWithoutLength(response, HttpBody.raw(options.transform(algorithm, opts)(readable), {
            contentType: response.headers["content-type"] ?? body.contentType
          })));
        }
      default:
        {
          return Effect.succeed(response);
        }
    }
  }
});
const streamBody = (response, evaluate, contentType) => setBodyWithoutLength(response, HttpBody.stream(Stream.fromReadableStream({
  evaluate,
  onError: identity
}), contentType));
const singleChunkStream = data => new ReadableStream({
  start(controller) {
    controller.enqueue(data);
    controller.close();
  }
});
const rawReadableStream = raw => {
  if (typeof ReadableStream !== "undefined" && raw instanceof ReadableStream) {
    return raw;
  } else if (raw instanceof globalThis.Response) {
    return raw.body ?? undefined;
  }
  return new globalThis.Response(raw).body ?? undefined;
};
/**
 * @internal
 */
export const compressionWeb = /*#__PURE__*/makeCompressionWeb({
  algorithms: ["gzip", "deflate"],
  transform: algorithm => compressionTransformWeb(algorithm)
});
/**
 * @internal
 */
export const defaultCompressible = contentType => {
  const semi = contentType.indexOf(";");
  const type = (semi === -1 ? contentType : contentType.slice(0, semi)).trim().toLowerCase();
  if (type.startsWith("text/")) {
    return true;
  }
  switch (type) {
    case "application/json":
    case "application/javascript":
    case "application/xml":
    case "image/svg+xml":
    case "application/wasm":
      {
        return true;
      }
  }
  return type.endsWith("+json") || type.endsWith("+xml");
};
const acceptMember = /^([a-z0-9!#$%&'*+.^_`|~-]+)(?:;q=(0(?:\.[0-9]{0,3})?|1(?:\.0{0,3})?))?$/;
/**
 * @internal
 */
export const parseAcceptEncoding = header => {
  const trimmed = header.trim();
  if (trimmed === "") {
    return undefined;
  }
  const accepted = new Map();
  for (const part of trimmed.split(",")) {
    const member = part.trim().toLowerCase().replace(/[ \t]*;[ \t]*/g, ";");
    const match = acceptMember.exec(member);
    if (match === null) {
      return undefined;
    }
    accepted.set(match[1], match[2] === undefined ? 1 : Number(match[2]));
  }
  return accepted;
};
/**
 * @internal
 */
export const negotiate = (header, preferred, supported) => {
  if (header === undefined) {
    return undefined;
  }
  const accepted = parseAcceptEncoding(header);
  if (accepted === undefined) {
    return undefined;
  }
  for (const algorithm of preferred) {
    if (!supported.has(algorithm)) {
      continue;
    }
    const quality = accepted.get(algorithm) ?? accepted.get("*");
    if (quality !== undefined && quality > 0) {
      return algorithm;
    }
  }
  return undefined;
};
//# sourceMappingURL=compression.js.map