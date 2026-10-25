/**
 * Serves static files for Effect HTTP applications.
 *
 * `HttpStaticServer` turns request paths into file responses under a configured
 * root directory. It can be used as an application value or mounted onto an
 * `HttpRouter`, and it handles index files, optional single-page application
 * fallback, MIME type headers, cache-control headers, byte ranges, and
 * conditional `304 Not Modified` responses.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Effect from "../Effect.js";
import * as FileSystem from "../FileSystem.js";
import * as Layer from "../Layer.js";
import * as Path from "../Path.js";
import * as HttpPlatform from "./HttpPlatform.js";
import * as HttpRouter from "./HttpRouter.js";
import * as HttpServerError from "./HttpServerError.js";
import * as HttpServerRequest from "./HttpServerRequest.js";
import * as HttpServerRespondable from "./HttpServerRespondable.js";
import * as HttpServerResponse from "./HttpServerResponse.js";
/**
 * Creates an `HttpApp` that serves files from a directory.
 *
 * **Example** (Serving files from a directory)
 *
 * ```ts import.meta.vitest
 * import { ByteSize, Effect, FileSystem, Layer, Path } from "effect"
 * import {
 *   HttpEffect,
 *   HttpPlatform,
 *   HttpServerResponse,
 *   HttpStaticServer
 * } from "effect/http"
 *
 * const TestFileSystem = FileSystem.layerNoop({
 *   stat: () =>
 *     Effect.succeed({
 *       type: "File",
 *       size: ByteSize.bytes(20)
 *     } as FileSystem.File.Info)
 * })
 * const TestHttpPlatform = Layer.succeed(
 *   HttpPlatform.HttpPlatform,
 *   HttpPlatform.HttpPlatform.of({
 *     platform: "web",
 *     fileResponse: (path) => Effect.succeed(HttpServerResponse.text(`Serving ${path}`)),
 *     fileWebResponse: () => Effect.die("unused")
 *   })
 * )
 * const TestServices = Layer.mergeAll(Path.layer, TestFileSystem, TestHttpPlatform)
 *
 * const program = Effect.gen(function*() {
 *   const app = yield* HttpStaticServer.make({ root: "/public" })
 *   const handler = HttpEffect.toWebHandler(app)
 *   const response = yield* Effect.promise(() => handler(new Request("http://localhost/guide.txt")))
 *   const body = yield* Effect.promise(() => response.text())
 *   return body
 * }).pipe(Effect.provide(TestServices))
 *
 * await Effect.runPromise(program) // => "Serving /public/guide.txt"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = /*#__PURE__*/Effect.fnUntraced(function* (options) {
  const fileSystem = yield* FileSystem.FileSystem;
  const path = yield* Path.Path;
  const platform = yield* HttpPlatform.HttpPlatform;
  const resolvedRoot = path.resolve(options.root);
  const index = "index" in options ? options.index : "index.html";
  const spa = options.spa === true;
  const cacheControl = options.cacheControl;
  const mimeTypes = {
    ...defaultMimeTypes,
    ...options.mimeTypes
  };
  const setFileHeaders = (response, filePath) => {
    let currentResponse = HttpServerResponse.setHeaders(response, {
      "Content-Type": resolveMimeType(path, filePath, mimeTypes),
      "Accept-Ranges": "bytes"
    });
    if (cacheControl !== undefined) {
      currentResponse = HttpServerResponse.setHeader(currentResponse, "Cache-Control", cacheControl);
    }
    return currentResponse;
  };
  const serveFile = Effect.fnUntraced(function* (request, filePath, fileSize) {
    const rangeHeader = request.method === "GET" ? request.headers["range"] : undefined;
    const shouldEvaluateConditionals = request.headers["if-none-match"] !== undefined || request.headers["if-modified-since"] !== undefined;
    let fullResponse;
    const getFullResponse = () => fullResponse === undefined ? Effect.map(handlePlatformError(request, platform.fileResponse(filePath)), response => setFileHeaders(response, filePath)) : Effect.succeed(fullResponse);
    if (shouldEvaluateConditionals) {
      fullResponse = yield* getFullResponse();
      const conditionalResponse = evaluateConditionalRequest(request, fullResponse);
      if (conditionalResponse !== undefined) {
        return conditionalResponse;
      }
    }
    if (rangeHeader === undefined) {
      return yield* getFullResponse();
    }
    const ifRange = request.headers["if-range"];
    if (ifRange !== undefined) {
      fullResponse = yield* getFullResponse();
      if (!matchesIfRange(ifRange, fullResponse.headers["etag"])) {
        return fullResponse;
      }
    }
    const resolvedFileSize = fileSize ?? (yield* handlePlatformError(request, fileSystem.stat(filePath))).size;
    const parsedRange = parseRange(rangeHeader, resolvedFileSize);
    if (parsedRange === undefined) {
      return yield* getFullResponse();
    }
    if (parsedRange === "unsatisfiable") {
      return HttpServerResponse.empty({
        status: 416,
        headers: {
          "Content-Range": `bytes */${resolvedFileSize}`
        }
      });
    }
    let response = setFileHeaders(yield* handlePlatformError(request, platform.fileResponse(filePath, {
      status: 206,
      offset: parsedRange.start,
      bytesToRead: parsedRange.end - parsedRange.start + BigInt(1)
    })), filePath);
    response = HttpServerResponse.setHeader(response, "Content-Range", `bytes ${parsedRange.start}-${parsedRange.end}/${resolvedFileSize}`);
    return response;
  });
  // @effect-diagnostics-next-line returnEffectInGen:off
  return HttpServerRequest.HttpServerRequest.use(request => {
    const resolvedPath = resolveFilePath(path, resolvedRoot, request.url);
    if (resolvedPath === undefined) {
      return Effect.fail(toRouteNotFoundError(request));
    }
    return Effect.matchEffect(fileSystem.stat(resolvedPath), {
      onFailure: error => error.reason._tag === "NotFound" && spa && index !== undefined && path.extname(resolvedPath) === "" && acceptsHtml(request.headers["accept"]) ? serveFile(request, path.join(resolvedRoot, index)) : error.reason._tag === "NotFound" ? Effect.fail(toRouteNotFoundError(request)) : Effect.fail(toInternalServerError(request, error)),
      onSuccess(info) {
        if (info.type === "File") {
          return serveFile(request, resolvedPath, info.size);
        }
        if (info.type === "Directory" && index !== undefined) {
          return serveFile(request, path.join(resolvedPath, index));
        }
        return Effect.fail(toRouteNotFoundError(request));
      }
    });
  });
});
/**
 * Creates a layer that mounts static files on an `HttpRouter`.
 *
 * **Example** (Mounting static files on a router)
 *
 * ```ts import.meta.vitest
 * import { Layer } from "effect"
 * import { HttpRouter, HttpServerResponse, HttpStaticServer } from "effect/http"
 *
 * const ApiLayer = HttpRouter.add("GET", "/health", HttpServerResponse.text("ok"))
 *
 * const StaticFilesLayer = HttpStaticServer.layer({
 *   root: "./public",
 *   prefix: "/static"
 * })
 *
 * const AppLayer = Layer.mergeAll(ApiLayer, StaticFilesLayer)
 * Layer.isLayer(AppLayer) // => true
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layer = options => Layer.effectDiscard(Effect.gen(function* () {
  const router = yield* HttpRouter.HttpRouter;
  const handler = (yield* make(options)).pipe(Effect.catch(HttpServerRespondable.toResponse));
  if (options.prefix !== undefined) {
    yield* router.prefixed(options.prefix).add("GET", "/*", handler);
    return;
  }
  yield* router.add("GET", "/*", handler);
}));
const defaultMimeTypes = {
  html: "text/html; charset=utf-8",
  htm: "text/html; charset=utf-8",
  css: "text/css; charset=utf-8",
  js: "text/javascript; charset=utf-8",
  mjs: "text/javascript; charset=utf-8",
  json: "application/json; charset=utf-8",
  xml: "application/xml; charset=utf-8",
  txt: "text/plain; charset=utf-8",
  csv: "text/csv; charset=utf-8",
  md: "text/markdown; charset=utf-8",
  yaml: "text/yaml; charset=utf-8",
  yml: "text/yaml; charset=utf-8",
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  gif: "image/gif",
  svg: "image/svg+xml; charset=utf-8",
  ico: "image/x-icon",
  webp: "image/webp",
  avif: "image/avif",
  woff: "font/woff",
  woff2: "font/woff2",
  ttf: "font/ttf",
  otf: "font/otf",
  eot: "application/vnd.ms-fontobject",
  mp3: "audio/mpeg",
  mp4: "video/mp4",
  webm: "video/webm",
  ogg: "audio/ogg",
  wav: "audio/wav",
  flac: "audio/flac",
  aac: "audio/aac",
  pdf: "application/pdf",
  zip: "application/zip",
  gz: "application/gzip",
  wasm: "application/wasm",
  map: "application/json",
  webmanifest: "application/manifest+json"
};
const stripQueryString = url => {
  const queryIndex = url.indexOf("?");
  return queryIndex === -1 ? url : url.slice(0, queryIndex);
};
const resolveMimeType = (path, filePath, mimeTypes) => {
  const extension = path.extname(filePath).toLowerCase();
  if (extension.length <= 1) {
    return "application/octet-stream";
  }
  return mimeTypes[extension.slice(1)] ?? "application/octet-stream";
};
const parseInteger = value => {
  if (!/^\d+$/.test(value)) {
    return undefined;
  }
  return BigInt(value);
};
const parseRange = (header, fileSize) => {
  const value = header.trim();
  if (!value.toLowerCase().startsWith("bytes=")) {
    return undefined;
  }
  const rangeValue = value.slice(6).trim();
  if (rangeValue.length === 0 || rangeValue.includes(",")) {
    return undefined;
  }
  const separatorIndex = rangeValue.indexOf("-");
  if (separatorIndex === -1) {
    return undefined;
  }
  const startPart = rangeValue.slice(0, separatorIndex).trim();
  const endPart = rangeValue.slice(separatorIndex + 1).trim();
  if (startPart === "" && endPart === "") {
    return undefined;
  }
  if (startPart === "") {
    const suffixLength = parseInteger(endPart);
    if (suffixLength === undefined) {
      return undefined;
    }
    if (suffixLength === BigInt(0) || fileSize === BigInt(0)) {
      return "unsatisfiable";
    }
    return {
      start: fileSize > suffixLength ? fileSize - suffixLength : BigInt(0),
      end: fileSize - BigInt(1)
    };
  }
  const start = parseInteger(startPart);
  if (start === undefined) {
    return undefined;
  }
  if (endPart === "") {
    if (start >= fileSize) {
      return "unsatisfiable";
    }
    return {
      start,
      end: fileSize - BigInt(1)
    };
  }
  const end = parseInteger(endPart);
  if (end === undefined) {
    return undefined;
  }
  if (start > end || start >= fileSize) {
    return "unsatisfiable";
  }
  return {
    start,
    end: end < fileSize ? end : fileSize - BigInt(1)
  };
};
const resolveFilePath = (path, root, url) => {
  const urlPath = stripQueryString(url);
  let decodedPath;
  try {
    decodedPath = decodeURIComponent(urlPath);
  } catch {
    return undefined;
  }
  if (decodedPath.includes("\u0000")) {
    return undefined;
  }
  const normalizedPath = path.normalize(decodedPath.startsWith("/") ? decodedPath.slice(1) : decodedPath);
  if (normalizedPath === ".." || normalizedPath.startsWith(`..${path.sep}`)) {
    return undefined;
  }
  const resolvedPath = path.join(root, normalizedPath);
  const rootPrefix = root.endsWith(path.sep) ? root : `${root}${path.sep}`;
  if (resolvedPath !== root && !resolvedPath.startsWith(rootPrefix)) {
    return undefined;
  }
  return resolvedPath;
};
const toRouteNotFoundError = request => new HttpServerError.HttpServerError({
  reason: new HttpServerError.RouteNotFound({
    request
  })
});
const toInternalServerError = (request, cause) => new HttpServerError.HttpServerError({
  reason: new HttpServerError.InternalError({
    request,
    cause
  })
});
const handlePlatformError = (request, self) => Effect.catchIf(self, error => error.reason._tag === "NotFound", () => Effect.fail(toRouteNotFoundError(request)), error => Effect.fail(toInternalServerError(request, error)));
const acceptsHtml = accept => accept !== undefined && accept.toLowerCase().includes("text/html");
const stripWeakEtagPrefix = value => {
  const trimmed = value.trim();
  return /^w\//i.test(trimmed) ? trimmed.slice(2) : trimmed;
};
const matchesIfNoneMatch = (ifNoneMatch, etag) => {
  const normalizedEtag = etag === undefined ? undefined : stripWeakEtagPrefix(etag);
  for (const candidate of ifNoneMatch.split(",")) {
    const value = candidate.trim();
    if (value === "") {
      continue;
    }
    if (value === "*") {
      return true;
    }
    if (normalizedEtag !== undefined && stripWeakEtagPrefix(value) === normalizedEtag) {
      return true;
    }
  }
  return false;
};
const isNotModifiedSince = (ifModifiedSince, lastModified) => {
  if (lastModified === undefined) {
    return false;
  }
  const ifModifiedSinceMs = Date.parse(ifModifiedSince);
  if (Number.isNaN(ifModifiedSinceMs)) {
    return false;
  }
  const lastModifiedMs = Date.parse(lastModified);
  if (Number.isNaN(lastModifiedMs)) {
    return false;
  }
  return lastModifiedMs <= ifModifiedSinceMs;
};
// Only a strong entity-tag can match (RFC 9110 §13.1.5). Weak tags start
// with `W/`, and Last-Modified dates never match: filesystem metadata cannot
// prove the file did not change twice within the same second.
const matchesIfRange = (ifRange, etag) => {
  const value = ifRange.trim();
  return value.startsWith("\"") && value === etag;
};
const notModifiedResponse = response => {
  const headers = {};
  const etag = response.headers["etag"];
  if (etag !== undefined) {
    headers["ETag"] = etag;
  }
  const cacheControl = response.headers["cache-control"];
  if (cacheControl !== undefined) {
    headers["Cache-Control"] = cacheControl;
  }
  const lastModified = response.headers["last-modified"];
  if (lastModified !== undefined) {
    headers["Last-Modified"] = lastModified;
  }
  return HttpServerResponse.empty({
    status: 304,
    headers
  });
};
const evaluateConditionalRequest = (request, response) => {
  const ifNoneMatch = request.headers["if-none-match"];
  if (ifNoneMatch !== undefined) {
    return matchesIfNoneMatch(ifNoneMatch, response.headers["etag"]) ? notModifiedResponse(response) : undefined;
  }
  const ifModifiedSince = request.headers["if-modified-since"];
  if (ifModifiedSince !== undefined && isNotModifiedSince(ifModifiedSince, response.headers["last-modified"])) {
    return notModifiedResponse(response);
  }
  return undefined;
};
//# sourceMappingURL=HttpStaticServer.js.map