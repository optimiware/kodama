/**
 * Scalar documentation UI for declarative `HttpApi` contracts.
 *
 * Use this module to mount a browser-based API reference on an `HttpRouter`
 * without writing or storing a separate OpenAPI file. The route renders an HTML
 * page containing the OpenAPI document produced from the supplied `HttpApi` and
 * boots Scalar in the browser.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Effect from "../Effect.js";
import * as Function from "../Function.js";
import * as HttpRouter from "../http/HttpRouter.js";
import * as HttpServerResponse from "../http/HttpServerResponse.js";
import * as Html from "./internal/html.js";
import * as internal from "./internal/httpApiScalar.js";
import * as OpenApi from "./OpenApi.js";
const makeHandler = options => {
  const makeResponse = Function.memoize(api => {
    const spec = OpenApi.fromApi(api);
    const {
      customFetch,
      ...scalar
    } = options.scalar ?? {};
    const scalarConfig = {
      _integration: "html",
      ...scalar
    };
    const scalarScript = options.source._tag === "Cdn" ? `<script src="${Html.escapeAttribute(`https://cdn.jsdelivr.net/npm/@scalar/api-reference@${encodeURIComponent(options.source.version ?? "latest")}/dist/browser/standalone.min.js`)}" crossorigin></script>` : `<script>${options.source.source}</script>`;
    return HttpServerResponse.html(`<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <title>${Html.escape(spec.info.title)}</title>
    ${!spec.info.description ? "" : `<meta name="description" content="${Html.escapeAttribute(spec.info.description)}"/>`}
    ${!spec.info.description ? "" : `<meta name="og:description" content="${Html.escapeAttribute(spec.info.description)}"/>`}
    <meta
      name="viewport"
      content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <div id="api-reference-container"></div>
    ${scalarScript}
    <script>
      window.Scalar.createApiReference(document.getElementById('api-reference-container'), {
        ...${Html.escapeJson(scalarConfig)},
        content: ${Html.escapeJson(spec)}${customFetch === undefined ? "" : `,
        customFetch: ${customFetch}`}
      })
    </script>
  </body>
</html>`);
  });
  return Effect.sync(() => makeResponse(options.api));
};
/**
 * Mounts a Scalar API reference page for an `HttpApi` using the bundled Scalar script.
 *
 * **Details**
 *
 * The route serves the OpenAPI specification generated from the API at the
 * configured path, defaulting to `/docs`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layer = (api, options) => HttpRouter.use(Effect.fnUntraced(function* (router) {
  const handler = makeHandler({
    api,
    source: {
      _tag: "Inline",
      source: internal.javascript
    },
    scalar: options?.scalar
  });
  yield* router.add("GET", options?.path ?? "/docs", handler);
}));
/**
 * Mounts a Scalar API reference page for an `HttpApi` that loads Scalar from jsDelivr.
 *
 * **Details**
 *
 * The route serves the OpenAPI specification generated from the API at the
 * configured path, defaulting to `/docs`; `version` selects the Scalar package
 * version loaded from the CDN.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerCdn = (api, options) => HttpRouter.use(Effect.fnUntraced(function* (router) {
  const handler = makeHandler({
    api,
    source: {
      _tag: "Cdn",
      version: options?.version
    },
    scalar: options?.scalar
  });
  yield* router.add("GET", options?.path ?? "/docs", handler);
}));
//# sourceMappingURL=HttpApiScalar.js.map