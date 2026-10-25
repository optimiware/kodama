/**
 * Swagger documentation UI for declarative `HttpApi` contracts.
 *
 * This module mounts a browser-based Swagger UI route on an `HttpRouter`. The
 * page renders the OpenAPI document derived from the supplied `HttpApi`, so a
 * running application can expose interactive API documentation without writing a
 * separate OpenAPI file.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Effect from "../Effect.js";
import * as Function from "../Function.js";
import * as HttpRouter from "../http/HttpRouter.js";
import * as HttpServerResponse from "../http/HttpServerResponse.js";
import * as Html from "./internal/html.js";
import * as internal from "./internal/httpApiSwagger.js";
import * as OpenApi from "./OpenApi.js";
const makeHandler = options => {
  const makeResponse = Function.memoize(api => {
    const spec = OpenApi.fromApi(api);
    return HttpServerResponse.html(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>${Html.escape(spec.info.title)} Documentation</title>
  <style>${internal.css}</style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script id="swagger-spec" type="application/json">
    ${Html.escapeJson(spec)}
  </script>
  <script>
    ${internal.javascript}
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        spec: JSON.parse(document.getElementById("swagger-spec").textContent),
        dom_id: "#swagger-ui",
      });
    };
  </script>
</body>
</html>`);
  });
  return Effect.sync(() => makeResponse(options.api));
};
/**
 * Mounts Swagger UI for an `HttpApi` at the configured path, defaulting to
 * `/docs`, using the OpenAPI specification generated from the API.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layer = (api, options) => HttpRouter.use(Effect.fnUntraced(function* (router) {
  const handler = makeHandler({
    api
  });
  yield* router.add("GET", options?.path ?? "/docs", handler);
}));
//# sourceMappingURL=HttpApiSwagger.js.map