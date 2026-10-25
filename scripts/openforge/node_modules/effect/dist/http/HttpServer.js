/**
 * Service for serving Effect HTTP responses on a concrete HTTP server.
 *
 * Platform adapters provide `HttpServer`, and routers or applications consume
 * it to run an `HttpServerResponse` effect for each incoming request. The
 * service exposes the listening address, while this module also includes helpers
 * for address formatting, server logging, and clients that target the current
 * server in tests.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as FileSystem from "../FileSystem.js";
import { dual } from "../Function.js";
import * as Layer from "../Layer.js";
import * as NetAddress from "../net/NetAddress.js";
import * as Path from "../Path.js";
import * as Etag from "./Etag.js";
import * as HttpClient from "./HttpClient.js";
import * as ClientRequest from "./HttpClientRequest.js";
import * as HttpPlatform from "./HttpPlatform.js";
/**
 * Service tag for an HTTP server runtime.
 *
 * **Details**
 *
 * The service can serve an HTTP response effect and exposes the address where the
 * server is listening.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class HttpServer extends /*#__PURE__*/Context.Service()("effect/http/HttpServer") {}
/**
 * Constructs an `HttpServer` service from a serving implementation and listening
 * address.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = options => options;
/**
 * Creates a layer that starts serving an HTTP response effect with the current
 * `HttpServer`.
 *
 * **Details**
 *
 * The request service is supplied by the server for each request; the returned
 * layer still requires the server, a scope, and any non-request dependencies of
 * the response effect or middleware.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const serve = /*#__PURE__*/dual(args => Effect.isEffect(args[0]), (effect, middleware) => Layer.effectDiscard(HttpServer.use(server => server.serve(effect, middleware))));
/**
 * Effect that starts serving an HTTP response effect with the current
 * `HttpServer`.
 *
 * **Details**
 *
 * The request service is supplied by the server for each request; the effect
 * requires a scope and any non-request dependencies of the response effect or
 * middleware.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export const serveEffect = /*#__PURE__*/dual(args => Effect.isEffect(args[0]), (effect, middleware) => HttpServer.use(server => server.serve(effect, middleware)));
/**
 * Formats a server address as a display string using {@link NetAddress.formatUrlUnsafe}.
 *
 * **Gotchas**
 *
 * Throws a `NetAddressError` when URL conversion fails, including for scoped
 * IPv6 addresses. Unix socket paths are displayed with a `unix://` prefix.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const formatAddress = NetAddress.formatUrlUnsafe;
/**
 * Reads the current server address, formats it with `formatAddress`, and passes
 * the formatted address to the supplied effectful function.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export const addressFormattedWith = f => Effect.flatMap(HttpServer, server => f(formatAddress(server.address)));
/**
 * Logs the formatted address of the current HTTP server.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export const logAddress = /*#__PURE__*/addressFormattedWith(_ => Effect.log(`Listening on ${_}`));
/**
 * Adds address logging to a layer that provides an `HttpServer`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const withLogAddress = layer => Layer.effectDiscard(logAddress).pipe(Layer.provideMerge(layer));
/**
 * Builds an `HttpClient` that sends requests to the current test HTTP server.
 *
 * **Details**
 *
 * For internet servers, requests are prefixed with the server URL and unspecified
 * addresses are replaced by IPv4 loopback, including dual-stack `::` listeners.
 *
 * **Gotchas**
 *
 * Unix socket addresses and scoped IPv6 addresses are not supported.
 *
 * @stability unstable
 * @category testing
 * @since 4.0.0
 */
export const makeTestClient = /*#__PURE__*/Effect.gen(function* () {
  const server = yield* HttpServer;
  const client = yield* HttpClient.HttpClient;
  const address = server.address;
  if (NetAddress.isUnixPathAddress(address)) {
    return yield* Effect.die(new Error("HttpServer.layerTestClient: UnixPathAddress not supported"));
  }
  const url = yield* Effect.fromResult(NetAddress.toUrl(address)).pipe(Effect.orDie);
  if (NetAddress.isUnspecified(address.address)) {
    url.hostname = NetAddress.formatIp(NetAddress.ipv4Loopback);
  }
  return HttpClient.mapRequest(client, ClientRequest.prependUrl(url.origin));
});
/**
 * Layer that provides the test `HttpClient` created by `makeTestClient`.
 *
 * @stability unstable
 * @category testing
 * @since 4.0.0
 */
export const layerTestClient = /*#__PURE__*/Layer.effect(HttpClient.HttpClient)(makeTestClient);
/**
 * Layer that provides the platform services commonly needed by HTTP
 * server tests.
 *
 * **Details**
 *
 * It includes `HttpPlatform`, `Path`, a weak ETag generator, and a no-op
 * `FileSystem`.
 *
 * @stability unstable
 * @category testing
 * @since 4.0.0
 */
export const layerServices = /*#__PURE__*/Layer.mergeAll(HttpPlatform.layer, Path.layer, Etag.layerWeak).pipe(/*#__PURE__*/Layer.provideMerge(/*#__PURE__*/FileSystem.layerNoop({})));
//# sourceMappingURL=HttpServer.js.map