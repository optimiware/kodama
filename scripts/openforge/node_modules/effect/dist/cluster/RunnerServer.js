import * as Effect from "../Effect.js";
import * as Fiber from "../Fiber.js";
import { constant } from "../Function.js";
import * as Layer from "../Layer.js";
import * as Option from "../Option.js";
import * as Queue from "../Queue.js";
import * as RpcServer from "../rpc/RpcServer.js";
import * as Message from "./Message.js";
import * as MessageStorage from "./MessageStorage.js";
import * as Reply from "./Reply.js";
import * as RunnerHealth from "./RunnerHealth.js";
import * as Runners from "./Runners.js";
import * as Sharding from "./Sharding.js";
import { ShardingConfig } from "./ShardingConfig.js";
const constVoid = /*#__PURE__*/constant(Effect.void);
const serializeDefectReply = (reply, defect, codecFor) => Effect.orDie(Reply.serialize(Reply.ReplyWithContext.fromDefect({
  id: reply.reply.id,
  requestId: reply.reply.requestId,
  defect
}), codecFor));
/**
 * Layer that handles runner protocol RPCs by forwarding requests to `Sharding`
 * and `MessageStorage`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerHandlers = /*#__PURE__*/Runners.Rpcs.toLayer(/*#__PURE__*/Effect.gen(function* () {
  const sharding = yield* Sharding.Sharding;
  const storage = yield* MessageStorage.MessageStorage;
  const {
    codecFor
  } = yield* RpcServer.Protocol;
  return {
    Ping: () => Effect.void,
    Notify: ({
      envelope,
      persisted
    }) => {
      const message = envelope._tag === "Request" ? new Message.IncomingRequest({
        envelope,
        respond: constVoid,
        lastSentReply: Option.none(),
        codecFor
      }) : new Message.IncomingEnvelope({
        envelope
      });
      return persisted ? sharding.notify(message) : sharding.send(message);
    },
    Effect: Effect.fnUntraced(function* ({
      persisted,
      request
    }) {
      const callerScope = yield* Effect.scope;
      let replyEncoded = Option.none();
      let resume = reply => {
        replyEncoded = Option.some(reply);
      };
      const message = new Message.IncomingRequest({
        envelope: request,
        lastSentReply: Option.none(),
        codecFor,
        callerScope,
        respond(reply) {
          resume(Reply.serializeOrDefect(reply, codecFor));
          return Effect.void;
        }
      });
      if (persisted) {
        return yield* Effect.callback(resume_ => {
          resume = resume_;
          const parent = Fiber.getCurrent();
          const onExit = exit => {
            if (exit._tag === "Failure") {
              resume(exit);
            }
          };
          const runFork = Effect.runForkWith(parent.context);
          const fiber = runFork(storage.registerReplyHandler(message));
          fiber.addObserver(onExit);
          runFork(Effect.catchTag(sharding.notify(message, constWaitUntilRead), "AlreadyProcessingMessage", () => Effect.void)).addObserver(onExit);
          return Fiber.interrupt(fiber);
        });
      }
      yield* sharding.send(message);
      return yield* Effect.callback(resume_ => {
        if (Option.isSome(replyEncoded)) {
          resume_(replyEncoded.value);
        } else {
          resume = resume_;
        }
      });
    }),
    Stream: Effect.fnUntraced(function* ({
      persisted,
      request
    }) {
      const callerScope = yield* Effect.scope;
      const queue = yield* Queue.make();
      const message = new Message.IncomingRequest({
        envelope: request,
        lastSentReply: Option.none(),
        codecFor,
        callerScope,
        respond(reply) {
          return Reply.serialize(reply, codecFor).pipe(Effect.flatMap(reply => {
            Queue.offerUnsafe(queue, reply);
            if (reply._tag === "WithExit") {
              Queue.endUnsafe(queue);
            }
            return Effect.void;
          }), Effect.catchTag("MalformedMessage", error => Effect.flatMap(serializeDefectReply(reply, error, codecFor), reply => {
            // the fallback defect reply is terminal, so end the stream
            Queue.offerUnsafe(queue, reply);
            Queue.endUnsafe(queue);
            return Effect.void;
          })));
        }
      });
      if (persisted) {
        yield* storage.registerReplyHandler(message).pipe(Effect.onError(cause => Queue.failCause(queue, cause)), Effect.forkScoped);
        yield* sharding.notify(message, constWaitUntilRead);
      } else {
        yield* sharding.send(message);
      }
      return queue;
    }),
    Envelope: ({
      envelope
    }) => sharding.send(new Message.IncomingEnvelope({
      envelope
    }))
  };
}));
const constWaitUntilRead = {
  waitUntilRead: true
};
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
export const layer = /*#__PURE__*/RpcServer.layer(Runners.Rpcs, {
  spanPrefix: "RunnerServer",
  disableTracing: true,
  disableFatalDefects: true
}).pipe(/*#__PURE__*/Layer.provide(layerHandlers));
/**
 * Layer that provides `RunnerServer` together with `Runners` and `Sharding`
 * clients.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerWithClients = /*#__PURE__*/layer.pipe(/*#__PURE__*/Layer.provideMerge(Sharding.layer), /*#__PURE__*/Layer.provideMerge(Runners.layerRpc));
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
export const layerClientOnly = /*#__PURE__*/Sharding.layer.pipe(/*#__PURE__*/Layer.provideMerge(Runners.layerRpc), /*#__PURE__*/Layer.provide(RunnerHealth.layerNoop), /*#__PURE__*/Layer.updateService(ShardingConfig, config => ({
  ...config,
  runnerAddress: Option.none()
})));
//# sourceMappingURL=RunnerServer.js.map