import type * as HttpApi from "../http-api/HttpApi.ts";
import type * as HttpApiGroup from "../http-api/HttpApiGroup.ts";
import * as Layer from "../Layer.ts";
import type * as Rpc from "../rpc/Rpc.ts";
import type * as Entity from "./Entity.ts";
import type { Sharding } from "./Sharding.ts";
/**
 * Creates HTTP API handlers for an entity proxy group.
 *
 * **Details**
 *
 * Each generated endpoint reads the `entityId` path parameter and forwards the
 * request payload to the corresponding entity client method, including discard
 * endpoints.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerHttpApi: <ApiId extends string, Groups extends HttpApiGroup.Constraint, Identifier extends HttpApiGroup.Identifier<Groups>, Type extends string, Rpcs extends Rpc.Any>(api: HttpApi.HttpApi<ApiId, Groups>, identifier: Identifier, entity: Entity.Entity<Type, Rpcs>) => Layer.Layer<HttpApiGroup.Service<ApiId, Identifier>, never, Sharding | Rpc.ServicesServer<Rpcs> | Rpc.ServicesClient<Rpcs>>;
/**
 * Creates RPC handlers for the group produced by `EntityProxy.toRpcGroup`.
 *
 * **Details**
 *
 * The handlers forward each prefixed proxy RPC to the target entity client using
 * the `entityId` embedded in the proxy payload.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerRpcHandlers: <const Type extends string, Rpcs extends Rpc.Any>(entity: Entity.Entity<Type, Rpcs>) => Layer.Layer<RpcHandlers<Rpcs, Type>, never, Sharding | Rpc.ServicesServer<Rpcs> | Rpc.ServicesClient<Rpcs>>;
/**
 * Union of RPC handler services required to serve the proxy RPCs for an entity.
 *
 * **Details**
 *
 * Includes both the normal prefixed RPC handler and its discard variant.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export type RpcHandlers<Rpcs extends Rpc.Any, Prefix extends string> = Rpcs extends Rpc.Rpc<infer _Tag, infer _Payload, infer _Success, infer _Error, infer _Middleware, infer _Requires> ? Rpc.Handler<`${Prefix}.${_Tag}`> | Rpc.Handler<`${Prefix}.${_Tag}Discard`> : never;
//# sourceMappingURL=EntityProxyServer.d.ts.map