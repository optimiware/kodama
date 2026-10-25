/**
 * Tests `HttpApi` implementations through an in-memory generated client.
 *
 * The helpers use the same request encoding, routing, response encoding, and
 * client decoding as the normal `HttpApiBuilder` and `HttpApiClient` pipeline,
 * but they do not start an HTTP server. This is useful for focused handler
 * tests, schema round trips, middleware behavior, and typed client calls.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as HttpClient from "../http/HttpClient.js";
import * as HttpRouter from "../http/HttpRouter.js";
import * as HttpServerRequest from "../http/HttpServerRequest.js";
import * as HttpServerResponse from "../http/HttpServerResponse.js";
import * as preResponseHandler from "../http/internal/preResponseHandler.js";
import * as Layer from "../Layer.js";
import * as HttpApiBuilder from "./HttpApiBuilder.js";
import * as HttpApiClient from "./HttpApiClient.js";
/**
 * Creates an in-memory client for testing selected groups of an `HttpApi`.
 *
 * **Details**
 *
 * Handlers for the selected groups are taken from the environment; unselected
 * groups are wired with placeholder handlers that fail if called.
 *
 * @stability unstable
 * @category testing
 * @since 4.0.0
 */
export const groups = /*#__PURE__*/Effect.fnUntraced(function* (api, groupIdentifiers, options) {
  let context = yield* Effect.context();
  const groups = api.groups;
  for (const identifier in groups) {
    const group = groups[identifier];
    if (groupIdentifiers.includes(identifier)) {
      continue;
    }
    const handlers = new Map();
    const routes = [];
    for (const endpointIdentifier in group.endpoints) {
      const endpoint = group.endpoints[endpointIdentifier];
      const handler = {
        endpoint: endpoint,
        handler: () => Effect.die(new Error(`Unhandled endpoint: ${endpointIdentifier}`)),
        isRaw: false,
        uninterruptible: false
      };
      handlers.set(endpointIdentifier, handler);
      routes.push(HttpApiBuilder.handlerToRoute(api, group, handler, context));
    }
    context = Context.add(context, group, {
      handlers,
      routes
    });
  }
  const layer = HttpApiBuilder.layer(api).pipe(Layer.provide(Layer.succeedContext(context)));
  const handler = yield* HttpRouter.toHttpEffect(layer);
  const httpClient = HttpClient.make(Effect.fnUntraced(function* (request) {
    const serverRequest = HttpServerRequest.fromClientRequest(request);
    const response = yield* handler.pipe(Effect.flatMap(response => {
      const preResponse = preResponseHandler.requestPreResponseHandlers.get(serverRequest.source);
      return preResponse === undefined ? Effect.succeed(response) : preResponse(serverRequest, response);
    }), Effect.provideService(HttpServerRequest.HttpServerRequest, serverRequest), Effect.orDie);
    return HttpServerResponse.toClientResponse(response, {
      request
    });
  }, Effect.scoped));
  return yield* HttpApiClient.makeWith(api, {
    httpClient,
    baseUrl: options?.baseUrl ?? "http://localhost:3000"
  });
});
//# sourceMappingURL=HttpApiTest.js.map