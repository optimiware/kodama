/**
 * Builds server-side routers for Effect HTTP applications.
 *
 * `HttpRouter` collects routes and middleware while an application layer is
 * being built. Once the router is complete, it handles each
 * `HttpServerRequest` by finding a matching route and producing an
 * `HttpServerResponse`. The module also includes helpers for route definitions,
 * prefixes, parameters, request decoding, CORS, and running the router.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import { compose, dual, identity } from "../Function.js";
import { fiberEnterInterruptibleUnsafe } from "../internal/effect.js";
import * as Layer from "../Layer.js";
import * as Option from "../Option.js";
import * as Schema from "../Schema.js";
import * as Scope from "../Scope.js";
import * as Tracer from "../Tracer.js";
import * as FindMyWay from "./FindMyWay.js";
import * as HttpEffect from "./HttpEffect.js";
import * as HttpIncomingMessage from "./HttpIncomingMessage.js";
import * as HttpMiddleware from "./HttpMiddleware.js";
import * as HttpServer from "./HttpServer.js";
import * as HttpServerError from "./HttpServerError.js";
import * as HttpServerRequest from "./HttpServerRequest.js";
import * as HttpServerResponse from "./HttpServerResponse.js";
const TypeId = "~effect/http/HttpRouter";
/**
 * Service tag for the HTTP router used while constructing an HTTP application.
 * Route and middleware layers require this service to register themselves with
 * the router.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export const HttpRouter = /*#__PURE__*/Context.Service("effect/http/HttpRouter");
/**
 * Constructs an empty `HttpRouter` service.
 *
 * **Details**
 *
 * The returned router accepts route and middleware registrations and later routes
 * the current `HttpServerRequest` to the matching `HttpServerResponse`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = /*#__PURE__*/Effect.gen(function* () {
  const router = FindMyWay.make(yield* RouterConfig);
  const middleware = new Set();
  const addAll = routes => Effect.contextWith(context => {
    const middleware = getMiddleware(context);
    const applyMiddleware = effect => {
      for (let i = 0; i < middleware.length; i++) {
        effect = middleware[i](effect);
      }
      return effect;
    };
    for (let i = 0; i < routes.length; i++) {
      const route = middleware.length === 0 ? routes[i] : makeRoute({
        ...routes[i],
        handler: applyMiddleware(routes[i].handler)
      });
      if (route.method === "*") {
        if (route.path.endsWith("/*")) {
          router.all(route.path, route);
          router.all(route.path.slice(0, -2), route);
        } else {
          router.all(route.path, route);
        }
      } else {
        if (route.path.endsWith("/*")) {
          router.on(route.method, route.path, route);
          router.on(route.method, route.path.slice(0, -2), route);
        } else {
          router.on(route.method, route.path, route);
        }
      }
    }
    return Effect.void;
  });
  return HttpRouter.of({
    [TypeId]: TypeId,
    prefixed(prefix) {
      prefix = removeTrailingSlash(prefix);
      return HttpRouter.of({
        ...this,
        prefixed: newPrefix => this.prefixed(prefixPath(newPrefix, prefix)),
        addAll: routes => addAll(routes.map(prefixRoute(prefix))),
        add: (method, path, handler, options) => addAll([makeRoute({
          method,
          path: prefixPath(path, prefix),
          handler: HttpServerResponse.isHttpServerResponse(handler) ? Effect.succeed(handler) : Effect.isEffect(handler) ? handler : Effect.flatMap(HttpServerRequest.HttpServerRequest, handler),
          uninterruptible: options?.uninterruptible ?? false,
          prefix
        })])
      });
    },
    addAll,
    add: (method, path, handler, options) => addAll([route(method, path, handler, options)]),
    addGlobalMiddleware: middleware_ => Effect.sync(() => {
      middleware.add(middleware_);
    }),
    asHttpEffect() {
      let handler = Effect.withFiber(fiber => {
        let context = fiber.context;
        const request = Context.getUnsafe(context, HttpServerRequest.HttpServerRequest);
        let result = router.find(request.method, request.url);
        if (result === undefined && request.method === "HEAD") {
          result = router.find("GET", request.url);
        }
        if (result === undefined) {
          return Effect.fail(new HttpServerError.HttpServerError({
            reason: new HttpServerError.RouteNotFound({
              request
            })
          }));
        }
        const route = result.handler;
        if (Option.isSome(route.prefix)) {
          context = Context.add(context, HttpServerRequest.HttpServerRequest, sliceRequestUrl(request, route.prefix.value));
        }
        context = Context.add(context, HttpServerRequest.ParsedSearchParams, result.searchParams);
        context = Context.add(context, RouteContext, {
          route,
          params: result.params
        });
        if (fiber.getRef(Tracer.Tracer) !== Tracer.nativeTracer) {
          const span = Context.getOrUndefined(context, Tracer.ParentSpan);
          if (span !== undefined && span._tag === "Span" && span.sampled) {
            span.attribute("http.route", route.path);
          }
        }
        // The enclosing request restores the fiber context on exit.
        fiber.setContext(context);
        if (!route.uninterruptible) {
          const interrupted = fiberEnterInterruptibleUnsafe(fiber);
          if (interrupted !== undefined) return interrupted;
        }
        return route.handler;
      });
      if (middleware.size === 0) return handler;
      for (const fn of Arr.reverse(middleware)) {
        handler = fn(handler);
      }
      return handler;
    }
  });
});
function sliceRequestUrl(request, prefix) {
  const prefexLen = prefix.length;
  return request.modify({
    url: request.url.length <= prefexLen ? "/" : request.url.slice(prefexLen)
  });
}
/**
 * Context reference for low-level router configuration.
 *
 * **Details**
 *
 * The value is passed to the route matcher when an `HttpRouter` is created and
 * defaults to an empty configuration.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export const RouterConfig = /*#__PURE__*/Context.Reference("effect/http/HttpRouter/RouterConfig", {
  defaultValue: () => ({})
});
/**
 * Service for the matched HTTP route in the current request.
 *
 * **When to use**
 *
 * Use to read captured path parameters and route metadata while handling a
 * request matched by the router.
 *
 * **Details**
 *
 * It provides the route definition and the path parameters captured by the route
 * matcher.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class RouteContext extends /*#__PURE__*/Context.Service()("effect/http/HttpRouter/RouteContext") {}
/**
 * Effect that returns the path parameters captured for the current matched route.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const params = /*#__PURE__*/Effect.withFiberSucceed(fiber => Context.getUnsafe(fiber.context, RouteContext).params);
/**
 * Decodes a schema from the current request and its JSON body.
 *
 * **Details**
 *
 * The input passed to the schema includes the request method, URL, headers,
 * cookies, path parameters, search parameters, and parsed JSON body. The effect
 * fails if the body cannot be parsed or the schema decode fails.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const schemaJson = (schema, options) => {
  const parse = Schema.decodeUnknownEffect(schema);
  const parseBody = HttpIncomingMessage.schemaBodyJson(Schema.Unknown, options);
  return Effect.contextWith(context => {
    const request = Context.get(context, HttpServerRequest.HttpServerRequest);
    const searchParams = Context.get(context, HttpServerRequest.ParsedSearchParams);
    const routeContext = Context.get(context, RouteContext);
    return Effect.flatMap(parseBody(request), body => parse({
      method: request.method,
      url: request.url,
      headers: request.headers,
      cookies: request.cookies,
      pathParams: routeContext.params,
      searchParams,
      body
    }, options));
  });
};
/**
 * Decodes a schema from the current request without reading the request body.
 *
 * **Details**
 *
 * The input passed to the schema includes the request method, URL, headers,
 * cookies, path parameters, and search parameters.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const schemaNoBody = (schema, options) => {
  const parse = Schema.decodeUnknownEffect(schema);
  return Effect.contextWith(context => {
    const request = Context.get(context, HttpServerRequest.HttpServerRequest);
    const searchParams = Context.get(context, HttpServerRequest.ParsedSearchParams);
    const routeContext = Context.get(context, RouteContext);
    return parse({
      method: request.method,
      url: request.url,
      headers: request.headers,
      cookies: request.cookies,
      pathParams: routeContext.params,
      searchParams
    }, options);
  });
};
/**
 * Decodes a schema from the current route path parameters and search parameters.
 *
 * **Details**
 *
 * When the same key appears in both sources, the path parameter value is used.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const schemaParams = (schema, options) => {
  const parse = Schema.decodeUnknownEffect(schema);
  return Effect.contextWith(context => {
    const searchParams = Context.get(context, HttpServerRequest.ParsedSearchParams);
    const routeContext = Context.get(context, RouteContext);
    return parse({
      ...searchParams,
      ...routeContext.params
    }, options);
  });
};
/**
 * Decodes a schema from the path parameters captured for the current matched
 * route.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const schemaPathParams = (schema, options) => {
  const parse = Schema.decodeUnknownEffect(schema);
  return Effect.flatMap(params, _ => parse(_, options));
};
/**
 * Creates a layer that accesses the current `HttpRouter` service and runs the
 * supplied effect.
 *
 * **When to use**
 *
 * Use when you need to register routes or middleware with the router during layer
 * construction.
 *
 * **Example** (Registering routes during layer construction)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { HttpRouter, HttpServerResponse } from "effect/http"
 *
 * const Routes = HttpRouter.use((router) =>
 *   router.add("GET", "/health", HttpServerResponse.text("ready"))
 * )
 *
 * const program = Effect.acquireUseRelease(
 *   Effect.sync(() => HttpRouter.toWebHandler(Routes, { disableLogger: true })),
 *   ({ handler }) =>
 *     Effect.gen(function*() {
 *       const response = yield* Effect.promise(() => handler(new Request("http://localhost/health")))
 *       const body = yield* Effect.promise(() => response.text())
 *       return body
 *     }),
 *   ({ dispose }) => Effect.promise(dispose)
 * )
 *
 * await Effect.runPromise(program) // => "ready"
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const use = f => Layer.effectDiscard(Effect.flatMap(HttpRouter, f));
/**
 * Create a layer that adds a single route to the HTTP router.
 *
 * **Example** (Adding a GET route)
 *
 * ```ts import.meta.vitest
 * import { Layer } from "effect"
 * import { HttpRouter, HttpServerResponse } from "effect/http"
 *
 * const Route = HttpRouter.add(
 *   "GET",
 *   "/hello",
 *   HttpServerResponse.text("Hello, World!")
 * )
 * Layer.isLayer(Route) // => true
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const add = (method, path, handler, options) => use(router => router.add(method, path, handler, options));
/**
 * Create a layer that adds multiple routes to the HTTP router.
 *
 * **Example** (Adding multiple routes)
 *
 * ```ts import.meta.vitest
 * import { Layer } from "effect"
 * import { HttpRouter, HttpServerResponse } from "effect/http"
 *
 * const Routes = HttpRouter.addAll([
 *   HttpRouter.route(
 *     "GET",
 *     "/hello",
 *     HttpServerResponse.text("Hello, World!")
 *   )
 * ])
 * Layer.isLayer(Routes) // => true
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const addAll = (routes, options) => Layer.effectDiscard(Effect.gen(function* () {
  const toAdd = Effect.isEffect(routes) ? yield* routes : routes;
  let router = yield* HttpRouter;
  if (options?.prefix) {
    router = router.prefixed(options.prefix);
  }
  yield* router.addAll(toAdd);
}));
/**
 * Layer that provides a newly constructed `HttpRouter`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layer = /*#__PURE__*/Layer.effect(HttpRouter)(make);
/**
 * Builds an application layer with a router and returns the router as an HTTP
 * handler effect.
 *
 * **Details**
 *
 * The returned effect handles the current `HttpServerRequest` in the current
 * `Scope`; route request markers are converted into the ordinary requirements of
 * the returned handler.
 *
 * **Gotchas**
 *
 * The layer is built with a forked `MemoMap`, so calling `toHttpEffect` twice
 * will build the same layer twice.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toHttpEffect = appLayer => Effect.contextWith(context => {
  const scope = Context.get(context, Scope.Scope);
  const memoMap = Layer.CurrentMemoMap.forkOrCreate(context);
  return Effect.map(Layer.buildWithMemoMap(Layer.provideMerge(appLayer, layer), memoMap, scope), context => {
    const router = Context.get(context, HttpRouter);
    return router.asHttpEffect();
  });
});
const RouteTypeId = "~effect/http/HttpRouter/Route";
const makeRoute = options => ({
  ...options,
  uninterruptible: options.uninterruptible ?? false,
  prefix: typeof options.prefix === "string" ? Option.some(options.prefix) : options.prefix ?? Option.none(),
  [RouteTypeId]: RouteTypeId
});
/**
 * Constructs a `Route` from an HTTP method, path, and handler.
 *
 * **Details**
 *
 * The handler may be a static response, an effect that produces a response, or a
 * function from the current request to a response effect. Set `uninterruptible` to
 * prevent the route handler from being made interruptible while it runs.
 *
 * @stability unstable
 * @category routes
 * @since 4.0.0
 */
export const route = (method, path, handler, options) => makeRoute({
  ...options,
  method,
  path,
  handler: HttpServerResponse.isHttpServerResponse(handler) ? Effect.succeed(handler) : Effect.isEffect(handler) ? handler : Effect.flatMap(HttpServerRequest.HttpServerRequest, handler),
  uninterruptible: options?.uninterruptible ?? false
});
const removeTrailingSlash = path => path.endsWith("/") ? path.slice(0, -1) : path;
/**
 * Adds a path prefix to a route path.
 *
 * **Details**
 *
 * Trailing slashes are removed from the prefix; `/` becomes the prefix itself and
 * `*` becomes a wildcard route under the prefix.
 *
 * @stability unstable
 * @category transforming
 * @since 4.0.0
 */
export const prefixPath = /*#__PURE__*/dual(2, (self, prefix) => {
  prefix = removeTrailingSlash(prefix);
  if (self === "*") return `${prefix}/*`;else if (self === "/") return prefix;
  return prefix + self;
});
/**
 * Returns a copy of a route with its path prefixed.
 *
 * **Details**
 *
 * The prefix is also tracked on the route so that, when the route handles a
 * request, the matched prefix can be removed from the request URL seen by the
 * handler.
 *
 * @stability unstable
 * @category routes
 * @since 4.0.0
 */
export const prefixRoute = /*#__PURE__*/dual(2, (self, prefix) => makeRoute({
  ...self,
  path: prefixPath(self.path, prefix),
  prefix: Option.match(self.prefix, {
    onNone: () => removeTrailingSlash(prefix),
    onSome: existingPrefix => prefixPath(existingPrefix, prefix)
  })
}));
const MiddlewareTypeId = "~effect/http/HttpRouter/Middleware";
/**
 * Create a middleware layer that can be used to modify requests and responses.
 *
 * **Details**
 *
 * By default, the middleware only affects the routes that it is provided to.
 *
 * If you want to create a middleware that applies globally to all routes, pass
 * the `global` option as `true`.
 *
 * **Example** (Applying route and global middleware)
 *
 * ```ts import.meta.vitest
 * import { Effect, Layer } from "effect"
 * import { HttpRouter, HttpServerResponse } from "effect/http"
 *
 * const RouteMiddleware = HttpRouter.middleware((httpEffect) =>
 *   Effect.map(httpEffect, HttpServerResponse.setHeader("x-route", "route"))
 * ).layer
 *
 * const GlobalMiddleware = HttpRouter.middleware(
 *   (httpEffect) => Effect.map(httpEffect, HttpServerResponse.setHeader("x-global", "global")),
 *   { global: true }
 * )
 *
 * const Routes = HttpRouter.add("GET", "/hello", HttpServerResponse.text("Hello")).pipe(
 *   Layer.provide(RouteMiddleware)
 * )
 * const App = Layer.mergeAll(Routes, GlobalMiddleware)
 *
 * const program = Effect.acquireUseRelease(
 *   Effect.sync(() => HttpRouter.toWebHandler(App, { disableLogger: true })),
 *   ({ handler }) =>
 *     Effect.gen(function*() {
 *       const response = yield* Effect.promise(() => handler(new Request("http://localhost/hello")))
 *       return [response.headers.get("x-route"), response.headers.get("x-global")]
 *     }),
 *   ({ dispose }) => Effect.promise(dispose)
 * )
 *
 * await Effect.runPromise(program) // => ["route", "global"]
 * ```
 *
 * @stability unstable
 * @category middleware
 * @since 4.0.0
 */
export const middleware = function () {
  if (arguments.length === 0) {
    return makeMiddleware;
  }
  return makeMiddleware(arguments[0], arguments[1]);
};
const makeMiddleware = (middleware, options) => options?.global ? Layer.effectDiscard(Effect.gen(function* () {
  const router = yield* HttpRouter;
  const fn = Effect.isEffect(middleware) ? yield* middleware : middleware;
  yield* router.addGlobalMiddleware(fn);
})) : new MiddlewareImpl(Effect.isEffect(middleware) ? Layer.effectContext(Effect.map(middleware, fn => Context.makeUnsafe(new Map([[fnContextKey, fn]])))) : Layer.succeedContext(Context.makeUnsafe(new Map([[fnContextKey, middleware]]))));
let middlewareId = 0;
const fnContextKey = "effect/http/HttpRouter/MiddlewareFn";
class MiddlewareImpl {
  [MiddlewareTypeId] = {};
  layerFn;
  dependencies;
  constructor(layerFn, dependencies) {
    this.layerFn = layerFn;
    this.dependencies = dependencies;
    const contextKey = `effect/http/HttpRouter/Middleware-${++middlewareId}`;
    this.layer = Layer.effectContext(Effect.gen({
      self: this
    }, function* () {
      const context = yield* Effect.context();
      const stack = [Context.getOrUndefinedUnsafe(context, fnContextKey)];
      if (this.dependencies) {
        const memoMap = yield* Layer.CurrentMemoMap;
        const scope = Context.get(context, Scope.Scope);
        const depsContext = yield* Layer.buildWithMemoMap(this.dependencies, memoMap, scope);
        stack.push(...getMiddleware(depsContext));
      }
      return Context.makeUnsafe(new Map([[contextKey, stack]]));
    })).pipe(Layer.provide(this.layerFn));
  }
  layer;
  combine(other) {
    return new MiddlewareImpl(this.layerFn, this.dependencies ? Layer.provideMerge(this.dependencies, other.layer) : other.layer);
  }
}
const middlewareCache = /*#__PURE__*/new WeakMap();
const getMiddleware = context => {
  let arr = middlewareCache.get(context);
  if (arr) return arr;
  const topLevel = Arr.empty();
  let maxLength = 0;
  for (const [key, value] of context.mapUnsafe) {
    if (key.startsWith("effect/http/HttpRouter/Middleware-")) {
      topLevel.push(value);
      if (value.length > maxLength) {
        maxLength = value.length;
      }
    }
  }
  if (topLevel.length === 0) {
    arr = [];
  } else {
    const middleware = new Set();
    for (let i = maxLength - 1; i >= 0; i--) {
      for (const arr of topLevel) {
        if (i < arr.length) {
          middleware.add(arr[i]);
        }
      }
    }
    arr = Arr.fromIterable(middleware).reverse();
  }
  middlewareCache.set(context, arr);
  return arr;
};
/**
 * Middleware that applies CORS headers to the HTTP response.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const cors = options => middleware(HttpMiddleware.cors(options), {
  global: true
});
/**
 * Middleware that disables the logger for some routes.
 *
 * **Example** (Disabling route logging)
 *
 * ```ts import.meta.vitest
 * import { Layer } from "effect"
 * import { HttpRouter, HttpServerResponse } from "effect/http"
 *
 * const Route = HttpRouter.add(
 *   "GET",
 *   "/hello",
 *   HttpServerResponse.text("Hello, World!")
 * ).pipe(
 *   // disable the logger for this route
 *   Layer.provide(HttpRouter.disableLogger)
 * )
 * Layer.isLayer(Route) // => true
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const disableLogger = /*#__PURE__*/middleware(HttpMiddleware.withLoggerDisabled).layer;
/**
 * Provides request-level dependencies to some routes.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const provideRequest = layer => self => Layer.provide(self, middleware()(Effect.gen(function* () {
  const services = yield* Layer.build(layer);
  return effect => Effect.provideContext(effect, services);
})).layer);
/**
 * Runs the provided application layer as an HTTP server.
 *
 * **Details**
 *
 * Layers first built inside the app are private to this server. Provide
 * services that must be shared with sibling layers outside `serve`.
 *
 * **Gotchas**
 *
 * The layer is built with a forked `MemoMap`, so calling `serve` twice will
 * build the same layer twice.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const serve = (appLayer, options) => {
  let middleware = options?.middleware;
  if (options?.disableLogger !== true) {
    middleware = middleware ? compose(middleware, HttpMiddleware.logger) : HttpMiddleware.logger;
  }
  const layerWithRouter = Layer.provideMerge(appLayer, options?.routerConfig ? Layer.provide(layer, Layer.succeed(RouterConfig)(options.routerConfig)) : layer);
  return Effect.contextWith(context => {
    const memoMap = Layer.CurrentMemoMap.forkOrCreate(context);
    return Layer.buildWithMemoMap(layerWithRouter, memoMap, Context.get(context, Scope.Scope));
  }).pipe(Effect.map(context => {
    const router = Context.get(context, HttpRouter);
    const handler = router.asHttpEffect();
    return Layer.merge(middleware ? HttpServer.serve(handler, middleware) : HttpServer.serve(handler), Layer.succeedContext(context));
  }), Layer.unwrap, options?.disableListenLog ? identity : HttpServer.withLogAddress);
};
/**
 * Builds a Fetch-compatible request handler from an HTTP router application
 * layer.
 *
 * **Details**
 *
 * The result contains a `handler` function that converts Web `Request` values to
 * Web `Response` values and a `dispose` function for releasing the layer
 * resources. The layer is built immediately, so the cost is paid when the
 * handler is created rather than on the first request. A layer that performs
 * asynchronous work while building may still be in progress when the first
 * request arrives, in which case that request waits for the build to finish.
 * If the build fails, every request rejects with the build error.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toWebHandler = (appLayer, options) => {
  let middleware = options?.middleware;
  if (options?.disableLogger !== true) {
    middleware = middleware ? compose(middleware, HttpMiddleware.logger) : HttpMiddleware.logger;
  }
  const RouterLayer = options?.routerConfig ? Layer.provide(layer, Layer.succeed(RouterConfig)(options.routerConfig)) : layer;
  return HttpEffect.toWebHandlerLayerWith(Layer.provideMerge(appLayer, RouterLayer), {
    toHandler: s => Effect.succeed(Context.get(s, HttpRouter).asHttpEffect()),
    middleware,
    memoMap: options?.memoMap
  });
};
//# sourceMappingURL=HttpRouter.js.map