import * as Layer from "../Layer.ts";
import type * as Rpc from "../rpc/Rpc.ts";
import * as RpcServer from "../rpc/RpcServer.ts";
import * as MessageStorage from "./MessageStorage.ts";
import * as RunnerHealth from "./RunnerHealth.ts";
import * as Runners from "./Runners.ts";
import type * as RunnerStorage from "./RunnerStorage.ts";
import * as Sharding from "./Sharding.ts";
import { ShardingConfig } from "./ShardingConfig.ts";
/**
 * Layer that handles runner protocol RPCs by forwarding requests to `Sharding`
 * and `MessageStorage`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerHandlers: Layer.Layer<Rpc.Handler<"Effect"> | Rpc.Handler<"Envelope"> | Rpc.Handler<"Notify"> | Rpc.Handler<"Ping"> | Rpc.Handler<"Stream">, never, MessageStorage.MessageStorage | RpcServer.Protocol | Sharding.Sharding>;
/**
 * Creates the runner RPC server layer, which receives messages from other
 * runners, forwards them to the `Sharding` layer, and responds to `Ping`
 * requests.
 *
 * **When to use**
 *
 * Use when a runner process should accept runner-to-runner protocol messages
 * over a provided server `RpcServer.Protocol`.
 *
 * **Gotchas**
 *
 * This layer does not choose or provide the wire transport; provide a
 * transport-specific `RpcServer.Protocol` separately.
 *
 * @see {@link layerHandlers} for the lower-level handler layer used when the RPC server is supplied elsewhere
 * @see {@link layerWithClients} for a runner server layer that also provides the `Sharding` and `Runners` clients
 * @see {@link layerClientOnly} for embedding a cluster client without serving runner RPCs
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<never, never, RpcServer.Protocol | Sharding.Sharding | MessageStorage.MessageStorage>;
/**
 * Layer that provides `RunnerServer` together with `Runners` and `Sharding`
 * clients.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerWithClients: Layer.Layer<Sharding.Sharding | Runners.Runners, never, RpcServer.Protocol | ShardingConfig | Runners.RpcClientProtocol | MessageStorage.MessageStorage | RunnerStorage.RunnerStorage | RunnerHealth.RunnerHealth>;
/**
 * Creates a client-only `Runners` layer.
 *
 * **When to use**
 *
 * Use to embed a cluster client inside another Effect application without registering with
 * the ShardManager or receiving shard assignments.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerClientOnly: Layer.Layer<Sharding.Sharding | Runners.Runners, never, ShardingConfig | Runners.RpcClientProtocol | MessageStorage.MessageStorage | RunnerStorage.RunnerStorage>;
//# sourceMappingURL=RunnerServer.d.ts.map