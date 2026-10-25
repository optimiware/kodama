import * as HttpRouter from "../http/HttpRouter.ts";
import type * as Layer from "../Layer.ts";
import type * as HttpApi from "./HttpApi.ts";
import type * as HttpApiGroup from "./HttpApiGroup.ts";
/**
 * Mounts Swagger UI for an `HttpApi` at the configured path, defaulting to
 * `/docs`, using the OpenAPI specification generated from the API.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: <Id extends string, Groups extends HttpApiGroup.Constraint>(api: HttpApi.HttpApi<Id, Groups>, options?: {
    readonly path?: `/${string}` | undefined;
}) => Layer.Layer<never, never, HttpRouter.HttpRouter>;
//# sourceMappingURL=HttpApiSwagger.d.ts.map