import * as Effect from "../Effect.ts";
import type { FileSystem } from "../FileSystem.ts";
import type { Generator } from "../http/Etag.ts";
import type { HttpPlatform } from "../http/HttpPlatform.ts";
import type { Path } from "../Path.ts";
import type { Scope } from "../Scope.ts";
import type * as HttpApi from "./HttpApi.ts";
import * as HttpApiClient from "./HttpApiClient.ts";
import type * as HttpApiEndpoint from "./HttpApiEndpoint.ts";
import type * as HttpApiGroup from "./HttpApiGroup.ts";
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
export declare const groups: <ApiId extends string, Groups extends HttpApiGroup.Constraint, const Identifiers extends ReadonlyArray<HttpApiGroup.Identifier<Groups>>, SelectedGroups extends HttpApiGroup.Constraint = Extract<Groups, {
    readonly identifier: Identifiers[number];
}>>(api: HttpApi.HttpApi<ApiId, Groups>, groupIdentifiers: Identifiers, options?: {
    readonly baseUrl?: string | URL | undefined;
} | undefined) => Effect.Effect<import("../Types.ts").Simplify<{ readonly [Group in Extract<Groups, {
    readonly topLevel: false;
}> as HttpApiGroup.Identifier<Group>]: HttpApiClient.Client.GroupByEndpoint<Group, never, never>; } & HttpApiClient.Client.TopLevelMethods<Groups, never, never>>, never, FileSystem | Generator | HttpPlatform | Path | Scope | HttpApiEndpoint.Middleware<HttpApiGroup.Endpoints<Groups>> | import("./HttpApiMiddleware.ts").MiddlewareClient<HttpApiEndpoint.Middleware<HttpApiGroup.Endpoints<Groups>>> | HttpApiGroup.ToService<ApiId, SelectedGroups>>;
//# sourceMappingURL=HttpApiTest.d.ts.map