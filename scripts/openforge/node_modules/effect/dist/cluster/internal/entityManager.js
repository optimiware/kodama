import * as Arr from "../../Array.js";
import * as Cause from "../../Cause.js";
import { Clock } from "../../Clock.js";
import * as Context from "../../Context.js";
import * as Duration from "../../Duration.js";
import * as Effect from "../../Effect.js";
import * as Equal from "../../Equal.js";
import * as Exit from "../../Exit.js";
import * as Fiber from "../../Fiber.js";
import { identity } from "../../Function.js";
import { scopeAddFinalizerUnsafe, scopeRemoveFinalizerUnsafe } from "../../internal/effect.js";
import * as Latch from "../../Latch.js";
import * as Metric from "../../Metric.js";
import * as Option from "../../Option.js";
import { CurrentLogAnnotations } from "../../References.js";
import * as RpcServer from "../../rpc/RpcServer.js";
import * as Schedule from "../../Schedule.js";
import * as Schema from "../../Schema.js";
import * as SchemaIssue from "../../SchemaIssue.js";
import * as Scope from "../../Scope.js";
import { AlreadyProcessingMessage, EntityNotAssignedToRunner, MailboxFull, MalformedMessage } from "../ClusterError.js";
import * as ClusterMetrics from "../ClusterMetrics.js";
import { isUninterruptibleForServer, Persisted, WithTransaction } from "../ClusterSchema.js";
import * as ClusterSchema from "../ClusterSchema.js";
import { CurrentAddress, CurrentRunnerAddress, KeepAliveLatch, KeepAliveRpc, Request } from "../Entity.js";
import * as Message from "../Message.js";
import * as MessageStorage from "../MessageStorage.js";
import * as Reply from "../Reply.js";
import { ShardingConfig } from "../ShardingConfig.js";
import * as Snowflake from "../Snowflake.js";
import { CurrentActivationScope } from "./entityActivation.js";
import { EntityReaper } from "./entityReaper.js";
import { acquireEntity, releaseEntity } from "./interruptors.js";
import { ResourceMap } from "./resourceMap.js";
import { ResourceRef } from "./resourceRef.js";
/**
 * @internal
 */
export const make = /*#__PURE__*/Effect.fnUntraced(function* (entity, buildHandlers, options) {
  const config = yield* ShardingConfig;
  const snowflakeGen = yield* Snowflake.Generator;
  const managerScope = yield* Effect.scope;
  const storageEnabled = options.storage !== MessageStorage.noop;
  const mailboxCapacity = options.mailboxCapacity ?? config.entityMailboxCapacity;
  const clock = yield* Clock;
  const context = yield* Effect.context();
  const defectRetryPolicy = options.defectRetryPolicy ? Schedule.concat(options.defectRetryPolicy, defaultRetryPolicy) : defaultRetryPolicy;
  const retryDriver = yield* Schedule.toStepWithSleep(defectRetryPolicy);
  const entityRpcs = new Map(entity.protocol.requests);
  // add internal rpcs
  entityRpcs.set(KeepAliveRpc._tag, KeepAliveRpc);
  const activeServers = new Map();
  const serverCloseLatches = new Map();
  const processedRequestIds = new Set();
  const entities = yield* ResourceMap.make(Effect.fnUntraced(function* (address) {
    if (!options.sharding.hasShardId(address.shardId)) {
      return yield* new EntityNotAssignedToRunner({
        address
      });
    }
    const scope = yield* Effect.scope;
    // Gate the spawn on the runner-wide entity cap. Registering the release
    // must be atomic with taking the slot, otherwise an interrupt in between
    // would leak it.
    yield* Effect.uninterruptible(Effect.suspend(() => options.residency.admitUnsafe() ? Scope.addFinalizer(scope, Effect.sync(options.residency.releaseUnsafe)) : Effect.fail(new MailboxFull({
      address
    }))));
    const endLatch = Latch.makeUnsafe();
    const keepAliveLatch = Latch.makeUnsafe();
    const closeLatches = {
      closed: Latch.makeUnsafe(),
      force: Latch.makeUnsafe()
    };
    yield* Scope.addFinalizer(scope, Effect.sync(() => {
      releaseEntity(address);
    }));
    // on shutdown, reset the storage for the entity
    yield* Scope.addFinalizerExit(scope, () => {
      serverCloseLatches.get(address)?.closed.openUnsafe();
      serverCloseLatches.delete(address);
      return Effect.void;
    });
    const activeRequests = new Map();
    const retired = Latch.makeUnsafe();
    const isActive = () => !retired.isOpen();
    // Replay previously dispatched requests before admitting new work.
    const replay = Effect.fnUntraced(function* (write) {
      if (!isActive()) return;
      for (const request of activeRequests.values()) {
        if (!request.delivered) continue;
        request.sentExit = false;
        yield* write(0, requestEnvelope(request), requestWriteOptions(request));
      }
    });
    const writeRef = yield* ResourceRef.from(scope, Effect.fnUntraced(function* (handlerScope) {
      let isShuttingDown = false;
      const handlerContext = context.pipe(Context.add(CurrentAddress, address), Context.add(CurrentRunnerAddress, options.runnerAddress), Context.add(KeepAliveLatch, keepAliveLatch), Context.add(CurrentActivationScope, scope), Context.add(Scope.Scope, handlerScope), Context.add(CurrentLogAnnotations, {}));
      // Initiate the behavior for the entity
      const handlers = yield* entity.protocol.toHandlers(buildHandlers).pipe(Effect.setContext(handlerContext), Effect.sandbox, Effect.tapError(cause => Effect.logError("Defect building entity handlers", cause)), Effect.retry(defectRetryPolicy));
      const server = yield* RpcServer.makeNoSerialization(entity.protocol, {
        spanPrefix: `${entity.type}(${address.entityId})`,
        spanAttributes: {
          ...options.spanAttributes,
          "entity.type": entity.type,
          "entity.id": address.entityId
        },
        concurrency: options.concurrency ?? 1,
        disableFatalDefects: options.disableFatalDefects,
        onFromServer(response) {
          switch (response._tag) {
            case "Exit":
              {
                const request = activeRequests.get(Snowflake.Snowflake(response.requestId));
                if (!request) return Effect.void;
                request.sentReply = true;
                request.sentExit = true;
                // Rebuild interrupts are not replies; replacement handlers replay the request.
                if (isShuttingDown && Exit.hasInterrupts(response.exit) && isActive()) {
                  return Effect.void;
                }
                // For durable messages, ignore interrupts during shutdown.
                // They will be retried when the entity is restarted.
                // Also, if the request is uninterruptible, we ignore the
                // interrupt.
                if (storageEnabled && Context.get(request.message.annotations, Persisted) && Exit.hasInterrupts(response.exit) && (isShuttingDown || isUninterruptibleForServer(request.message.annotations))) {
                  if (!isShuttingDown) {
                    request.sentExit = false;
                    return server.write(0, requestEnvelope(request), requestWriteOptions(request)).pipe(Effect.forkIn(handlerScope));
                  }
                  activeRequests.delete(Snowflake.Snowflake(response.requestId));
                  return options.storage.unregisterReplyHandler(request.message.envelope.requestId);
                }
                return retryRespond(4, Effect.suspend(() => request.message.respond(new Reply.WithExit({
                  requestId: Snowflake.Snowflake(response.requestId),
                  id: snowflakeGen.nextUnsafe(),
                  exit: response.exit
                })))).pipe(Effect.flatMap(() => {
                  if (storageEnabled) {
                    processedRequestIds.add(request.message.envelope.requestId);
                  }
                  activeRequests.delete(Snowflake.Snowflake(response.requestId));
                  // Start the idle timer when the last request completes.
                  if (activeRequests.size === 0) {
                    state.lastActiveCheck = clock.currentTimeMillisUnsafe();
                  }
                  return Effect.void;
                }), Effect.orDie);
              }
            case "Chunk":
              {
                const request = activeRequests.get(Snowflake.Snowflake(response.requestId));
                if (!request) return Effect.void;
                const sequence = request.sequence;
                request.sequence++;
                if (!request.sentReply) {
                  request.sentReply = true;
                }
                return Effect.orDie(retryRespond(4, Effect.suspend(() => {
                  const reply = new Reply.Chunk({
                    requestId: Snowflake.Snowflake(response.requestId),
                    id: snowflakeGen.nextUnsafe(),
                    sequence,
                    values: response.values
                  });
                  request.lastSentChunk = Option.some(reply);
                  return request.message.respond(reply);
                })));
              }
            case "Defect":
              {
                if (!isActive()) return endLatch.open;
                const rebuild = writeRef.rebuildUnsafe({
                  from: server.write,
                  prepare: replay
                });
                if (!rebuild) return Effect.void;
                return Effect.forkIn(restart(Cause.die(response.defect), rebuild), managerScope);
              }
            case "ClientEnd":
              {
                return endLatch.open;
              }
          }
        }
      }).pipe(Scope.provide(handlerScope), Effect.setContext(Context.merge(handlerContext, handlers)));
      yield* Scope.addFinalizer(handlerScope, Effect.sync(() => {
        isShuttingDown = true;
      }));
      return server.write;
    }), address);
    function restart(cause, rebuild) {
      return Effect.logError("Defect in entity, restarting", cause).pipe(Effect.andThen(Effect.ignore(retryDriver(void 0))), Effect.flatMap(() => isActive() ? rebuild : endLatch.open), Effect.annotateLogs({
        module: "EntityManager",
        address,
        runner: options.runnerAddress
      }), Effect.catchCause(cause => {
        if (Cause.hasInterruptsOnly(cause)) return Effect.void;
        const retry = isActive() ? writeRef.rebuildUnsafe({
          prepare: replay
        }) : undefined;
        return retry ? restart(cause, retry) : Effect.void;
      }));
    }
    const state = {
      scope,
      address,
      write(clientId, message, writeOptions) {
        return Effect.suspend(() => {
          if (message._tag !== "Request") {
            // Interrupts, acks and EOF reach the handlers even during shutdown.
            const write = writeRef.getUnsafe();
            return write ? write(clientId, message, writeOptions) : Effect.flatMap(writeRef.await, write => write(clientId, message, writeOptions));
          }
          if (!isActive()) return Effect.interrupt;
          const write = writeRef.getUnsafe();
          if (write === undefined) {
            return Effect.flatMap(Effect.raceFirst(writeRef.await, retired.await), () => state.write(clientId, message, writeOptions));
          }
          const request = activeRequests.get(Snowflake.Snowflake(message.id));
          if (!request) return Effect.void;
          request.delivered = true;
          return write(clientId, message, writeOptions);
        });
      },
      activeRequests,
      lastActiveCheck: clock.currentTimeMillisUnsafe(),
      keepAliveLatch,
      keepAliveEnabled: false
    };
    // During shutdown, signal that no more messages will be processed
    // and wait for the fiber to complete.
    //
    // If the termination timeout is reached, let the server clean itself up
    yield* Scope.addFinalizer(scope, Effect.suspend(() => {
      activeServers.delete(address.entityId);
      retired.openUnsafe();
      acquireEntity(address);
      return Effect.raceFirst(state.write(0, {
        _tag: "Eof"
      }).pipe(Effect.andThen(endLatch.await), Effect.timeoutOption(config.entityTerminationTimeout), Effect.interruptible), Effect.interruptible(closeLatches.force.await));
    }));
    if (!options.sharding.hasShardId(address.shardId)) {
      return yield* new EntityNotAssignedToRunner({
        address
      });
    }
    // Do not make shard interruption wait for an entity that is still building.
    serverCloseLatches.set(address, closeLatches);
    activeServers.set(address.entityId, state);
    return state;
  }, Effect.provideService(CurrentLogAnnotations, {})));
  const reaper = yield* EntityReaper;
  const maxIdleTime = Duration.toMillis(Duration.fromInputUnsafe(options.maxIdleTime ?? config.entityMaxIdleTime));
  if (Number.isFinite(maxIdleTime)) {
    yield* reaper.register({
      maxIdleTime,
      servers: activeServers,
      entities
    });
  }
  // update metrics for active servers
  const typeAttributes = Metric.CurrentMetricAttributes.context({
    type: entity.type
  });
  yield* Effect.sync(() => {
    ClusterMetrics.entities.updateUnsafe(BigInt(activeServers.size), typeAttributes);
  }).pipe(Effect.andThen(Effect.sleep(1000)), Effect.forever, Effect.forkIn(managerScope));
  function sendLocal(message) {
    return Effect.provideService(Effect.flatMap(entities.get(message.envelope.address), server => {
      switch (message._tag) {
        case "IncomingRequestLocal":
          {
            // If the request is already running, then we might have more than
            // one sender for the same request. In this case, the other senders
            // should resume from storage only.
            let entry = server.activeRequests.get(message.envelope.requestId);
            if (entry || processedRequestIds.has(message.envelope.requestId)) {
              return Effect.fail(new AlreadyProcessingMessage({
                envelopeId: message.envelope.requestId,
                address: message.envelope.address
              }));
            }
            const rpc = entityRpcs.get(message.envelope.tag);
            if (!storageEnabled && Context.get(message.annotations, Persisted)) {
              return Effect.die("EntityManager.sendLocal: Cannot process a persisted message without MessageStorage");
            }
            // Cluster internal RPCs
            // keep-alive RPC
            if (rpc._tag === KeepAliveRpc._tag) {
              const msg = message;
              const reply = Effect.suspend(() => Effect.orDie(retryRespond(4, msg.respond(new Reply.WithExit({
                requestId: message.envelope.requestId,
                id: snowflakeGen.nextUnsafe(),
                exit: Exit.void
              })))));
              if (server.keepAliveEnabled) return reply;
              server.keepAliveEnabled = true;
              return server.keepAliveLatch.whenOpen(Effect.suspend(() => {
                server.keepAliveEnabled = false;
                return reply;
              })).pipe(Effect.forkIn(server.scope, {
                startImmediately: true
              }), Effect.asVoid);
            }
            if (mailboxCapacity !== "unbounded" && server.activeRequests.size >= mailboxCapacity) {
              return Effect.fail(new MailboxFull({
                address: message.envelope.address
              }));
            }
            const callerScope = message.callerScope !== undefined && !Context.get(message.annotations, Persisted) && Context.get(message.annotations, ClusterSchema.Uninterruptible) === false ? message.callerScope : undefined;
            if (callerScope?.state._tag === "Closed") {
              return Effect.void;
            }
            entry = {
              rpc,
              message,
              sentReply: false,
              delivered: false,
              sentExit: false,
              lastSentChunk: Option.filter(message.lastSentReply, reply => reply._tag === "Chunk"),
              sequence: Option.match(message.lastSentReply, {
                onNone: () => 0,
                onSome: reply => reply._tag === "Chunk" ? reply.sequence + 1 : 0
              }),
              callerScope
            };
            server.activeRequests.set(message.envelope.requestId, entry);
            if (callerScope !== undefined) {
              // Register cleanup atomically with admission, including requests awaiting delivery.
              scopeAddFinalizerUnsafe(callerScope, {}, () => Effect.sync(() => {
                if (server.activeRequests.delete(message.envelope.requestId) && server.activeRequests.size === 0) {
                  server.lastActiveCheck = clock.currentTimeMillisUnsafe();
                }
              }));
            }
            return server.write(0, requestEnvelope(entry), requestWriteOptions(entry));
          }
        case "IncomingEnvelope":
          {
            const entry = server.activeRequests.get(message.envelope.requestId);
            if (!entry) {
              return Effect.void;
            } else if (message.envelope._tag === "AckChunk" && Option.isSome(entry.lastSentChunk) && message.envelope.replyId !== entry.lastSentChunk.value.id) {
              return Effect.void;
            }
            return server.write(0, message.envelope._tag === "AckChunk" ? {
              _tag: "Ack",
              requestId: message.envelope.requestId
            } : {
              _tag: "Interrupt",
              requestId: message.envelope.requestId,
              interruptors: []
            });
          }
      }
    }), CurrentLogAnnotations, {});
  }
  // Bind each handler fiber, including replays, before its body runs.
  const bindToCaller = callerScope => effect => Effect.withFiber(fiber => {
    if (callerScope.state._tag === "Closed") return Effect.interrupt;
    const key = {};
    scopeAddFinalizerUnsafe(callerScope, key, () => Effect.sync(() => fiber.interruptUnsafe(fiber.id)));
    return Effect.ensuring(effect, Effect.sync(() => scopeRemoveFinalizerUnsafe(callerScope, key)));
  });
  const requestWriteOptions = entry => {
    const onTransaction = Context.get(entry.message.annotations, WithTransaction) ? options.storage.withTransaction : undefined;
    const onCaller = entry.callerScope && bindToCaller(entry.callerScope);
    if (!onCaller) return onTransaction && {
      onRequest: onTransaction
    };
    return {
      onRequest: onTransaction ? effect => onCaller(onTransaction(effect)) : onCaller
    };
  };
  const decodeMessage = makeMessageDecode(entityRpcs);
  const runFork = Effect.runForkWith(context);
  return identity({
    interruptShard: (shardId, options) => Effect.suspend(function loop() {
      const fibers = Arr.empty();
      if (options?.force === true) {
        serverCloseLatches.forEach((latches, address) => {
          if (shardId[Equal.symbol](address.shardId)) {
            latches.force.openUnsafe();
          }
        });
      }
      activeServers.forEach(state => {
        if (shardId[Equal.symbol](state.address.shardId)) {
          fibers.push(runFork(entities.removeIgnore(state.address)));
        }
      });
      serverCloseLatches.forEach((latches, address) => {
        if (shardId[Equal.symbol](address.shardId)) {
          fibers.push(runFork(latches.closed.await));
        }
      });
      if (fibers.length === 0) return Effect.void;
      return Effect.flatMap(Fiber.joinAll(fibers), loop);
    }),
    isProcessingFor(message, options) {
      if (options?.excludeReplies !== true && options?.excludeCompleted !== true && processedRequestIds.has(message.envelope.requestId)) {
        return true;
      }
      const state = activeServers.get(message.envelope.address.entityId);
      if (!state) return false;
      const request = state.activeRequests.get(message.envelope.requestId);
      if (request === undefined) {
        return false;
      } else if (options?.excludeReplies && request.sentReply) {
        return false;
      } else if (options?.excludeCompleted && request.sentExit) {
        return false;
      }
      return true;
    },
    clearProcessed() {
      processedRequestIds.clear();
    },
    isResidentUnsafe: address => entities.hasUnsafe(address),
    residentAddressesUnsafe: () => entities.keysUnsafe(),
    sendLocal,
    send: message => decodeMessage(message).pipe(Effect.matchEffect({
      onFailure: cause => {
        if (message._tag === "IncomingEnvelope") {
          return Effect.die(new MalformedMessage({
            cause
          }));
        }
        return Effect.orDie(message.respond(new Reply.ReplyWithContext({
          reply: new Reply.WithExit({
            id: snowflakeGen.nextUnsafe(),
            requestId: message.envelope.requestId,
            exit: Exit.die(new MalformedMessage({
              cause
            }))
          }),
          rpc: entityRpcs.get(message.envelope.tag),
          context: context
        })));
      },
      onSuccess: decoded => {
        if (decoded._tag === "IncomingEnvelope") {
          return sendLocal(decoded);
        }
        const request = message;
        const rpc = entityRpcs.get(decoded.envelope.tag);
        return sendLocal(new Message.IncomingRequestLocal({
          annotations: Context.get(rpc.annotations, ClusterSchema.Dynamic)(rpc.annotations, decoded.envelope),
          envelope: decoded.envelope,
          lastSentReply: decoded.lastSentReply,
          callerScope: request.callerScope,
          respond: reply => request.respond(new Reply.ReplyWithContext({
            reply,
            rpc,
            context: context
          }))
        }));
      }
    }), Effect.provideContext(context)),
    activeEntityCount: Effect.sync(() => activeServers.size)
  });
});
const defaultRetryPolicy = /*#__PURE__*/Schedule.min([/*#__PURE__*/Schedule.exponential(500, 1.5), /*#__PURE__*/Schedule.spaced("10 seconds")]);
const makeMessageDecode = entityRpcs => {
  const decodeRequest = Effect.fnUntracedEager(function* (message, rpc) {
    const codecFor = message.codecFor;
    const payload = yield* Schema.decodeEffect(codecFor(rpc.payloadSchema))(message.envelope.payload);
    const lastSentReply = Option.isNone(message.lastSentReply) ? message.lastSentReply : Option.some(yield* Schema.decodeEffect(Reply.Reply(rpc, codecFor))(message.lastSentReply.value));
    return {
      _tag: "IncomingRequest",
      envelope: {
        ...message.envelope,
        payload
      },
      lastSentReply
    };
  });
  return message => {
    if (message._tag === "IncomingEnvelope") {
      return Effect.succeed(message);
    }
    const rpc = entityRpcs.get(message.envelope.tag);
    if (!rpc) {
      return Effect.fail(new Schema.SchemaError(new SchemaIssue.InvalidValue({
        message: "Expected a known entity RPC tag"
      })));
    }
    return decodeRequest(message, rpc);
  };
};
const requestEnvelope = entry => ({
  ...entry.message.envelope,
  id: entry.message.envelope.requestId,
  tag: entry.message.envelope.tag,
  payload: new Request({
    ...entry.message.envelope,
    lastSentChunk: entry.lastSentChunk
  })
});
const retryRespond = (times, effect) => times === 0 ? effect : Effect.catch(effect, () => Effect.delay(retryRespond(times - 1, effect), 200));
//# sourceMappingURL=entityManager.js.map