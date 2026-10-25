import * as Arr from "../Array.js";
import * as Cache from "../Cache.js";
import * as Context from "../Context.js";
import * as Data from "../Data.js";
import * as Effect from "../Effect.js";
import * as Equal from "../Equal.js";
import * as Hash from "../Hash.js";
import * as Layer from "../Layer.js";
import * as Option from "../Option.js";
import * as Redacted from "../Redacted.js";
import * as Stream from "../Stream.js";
import * as EventLog from "./EventLog.js";
import { ChunkedMessage, EventLogAuthentication, EventLogProtocolError, EventLogRemoteRpcs, HelloResponse, SingleMessage } from "./EventLogMessage.js";
import * as EventLogSessionAuth from "./EventLogSessionAuth.js";
/**
 * Provides RPC authentication middleware that reads the authenticated
 * `EventLog.Identity` from client annotations.
 *
 * **Details**
 *
 * Requests without an identity fail with a forbidden `EventLogProtocolError`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerAuthMiddleware = /*#__PURE__*/Layer.succeed(EventLogAuthentication, (effect, {
  client,
  rpc
}) => {
  const identity = Context.getOrUndefined(client.annotations, EventLog.Identity);
  if (identity) return Effect.provideService(effect, EventLog.Identity, identity);
  return Effect.fail(new EventLogProtocolError({
    requestTag: rpc._tag,
    publicKey: undefined,
    code: "Forbidden",
    message: "Unauthenticated request"
  }));
});
/**
 * Creates the shared RPC handlers for the event-log remote protocol.
 *
 * **Details**
 *
 * The layer manages hello challenges, verifies session authentication, reassembles
 * chunked writes, delegates write and change handling to the supplied callbacks,
 * and frames large change payloads into chunks.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerRpcHandlers = options => EventLogRemoteRpcs.toLayer(Effect.gen(function* () {
  const clientChallenges = yield* Cache.make({
    lookup: _clientId => Effect.orDie(EventLogSessionAuth.makeSessionAuthChallenge),
    capacity: Number.MAX_SAFE_INTEGER,
    timeToLive: EventLogSessionAuth.SessionAuthChallengeTimeToLiveMillis
  });
  let chunkedIdCounter = 0;
  const persistedSigningPublicKeys = yield* Cache.make({
    lookup: key => options.getOrCreateSessionAuthBinding(key.publicKey, key.signingPublicKey).pipe(Effect.catchCause(_ => Effect.fail(new EventLogProtocolError({
      requestTag: "Authenticate",
      publicKey: key.publicKey,
      code: "Forbidden",
      message: "Session auth binding lookup failed"
    })))),
    capacity: 4096
  });
  return EventLogRemoteRpcs.of({
    "EventLog.Hello": Effect.fnUntraced(function* (_, {
      client
    }) {
      const challenge = yield* Cache.get(clientChallenges, client.id);
      return new HelloResponse({
        remoteId: options.remoteId,
        challenge
      });
    }),
    "EventLog.Authenticate": Effect.fnUntraced(function* (request, {
      client
    }) {
      const challenge = Option.getOrNull(yield* Cache.getOption(clientChallenges, client.id));
      if (!challenge) {
        return yield* new EventLogProtocolError({
          requestTag: "Authenticate",
          publicKey: request.publicKey,
          code: "Forbidden",
          message: "Session auth challenge has expired"
        });
      }
      yield* Cache.invalidate(clientChallenges, client.id);
      if (options.authorizeIdentity) {
        yield* options.authorizeIdentity(request.publicKey);
      }
      const signingPublicKey = yield* Cache.get(persistedSigningPublicKeys, new SessionAuthCacheKey({
        publicKey: request.publicKey,
        signingPublicKey: request.signingPublicKey
      }));
      const verified = yield* EventLogSessionAuth.verifySessionAuthenticateRequest({
        remoteId: options.remoteId,
        challenge,
        publicKey: request.publicKey,
        signingPublicKey,
        signature: request.signature,
        algorithm: request.algorithm
      }).pipe(Effect.catch(() => Effect.succeed(false)));
      if (!verified) {
        return yield* new EventLogProtocolError({
          requestTag: "Authenticate",
          publicKey: request.publicKey,
          code: "Forbidden",
          message: "Session auth signature verification failed"
        });
      }
      const authenticatedIdentities = new Set(Context.getOrUndefined(client.annotations, AuthenticatedIdentities));
      authenticatedIdentities.add(request.publicKey);
      void client.annotate(EventLog.Identity, {
        publicKey: request.publicKey,
        privateKey: constEmptyPrivateKey
      }).annotate(AuthenticatedIdentities, authenticatedIdentities).annotate(ChunkedMessageState, new Map());
    }),
    "EventLog.WriteSingle": Effect.fnUntraced(function* (request, {
      client
    }) {
      yield* options.onWrite(request.data, Context.getOrUndefined(client.annotations, AuthenticatedIdentities) ?? new Set());
    }),
    "EventLog.WriteChunked": Effect.fnUntraced(function* (request, {
      client
    }) {
      const state = Context.get(client.annotations, ChunkedMessageState);
      const data = ChunkedMessage.join(state, request);
      if (!data) return;
      yield* options.onWrite(data, Context.getOrUndefined(client.annotations, AuthenticatedIdentities) ?? new Set());
    }),
    "EventLog.Changes": (request, {
      client
    }) => {
      const authenticatedIdentities = Context.getOrUndefined(client.annotations, AuthenticatedIdentities);
      if (!authenticatedIdentities?.has(request.publicKey)) {
        return Stream.fail(new EventLogProtocolError({
          requestTag: "Changes",
          publicKey: request.publicKey,
          code: "Forbidden",
          message: "Identity is not authenticated"
        }));
      }
      return options.changes({
        publicKey: request.publicKey,
        storeId: request.storeId,
        startSequence: request.startSequence
      }).pipe(Stream.mapArray(Arr.flatMap(data => {
        if (data.byteLength <= ChunkedMessage.chunkSize) {
          return [new SingleMessage({
            data
          })];
        }
        return ChunkedMessage.split(chunkedIdCounter++, data);
      })), Stream.catchCause(_ => Stream.fail(new EventLogProtocolError({
        requestTag: "Changes",
        publicKey: request.publicKey,
        code: "InternalServerError",
        message: "Decoding failure"
      }))));
    }
  });
})).pipe(Layer.merge(layerAuthMiddleware));
/**
 * Annotation that stores partial `ChunkedMessage` data while chunked writes are
 * being reassembled.
 *
 * **When to use**
 *
 * Use to keep per-client chunk assembly state while handling chunked event-log
 * writes.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class ChunkedMessageState extends /*#__PURE__*/Context.Reference("effect/eventlog/EventLogServer/ChunkedMessageState", {
  defaultValue: () => new Map()
}) {}
/**
 * Annotation containing the public keys authenticated on an RPC connection.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class AuthenticatedIdentities extends /*#__PURE__*/Context.Service()("effect/eventlog/EventLogServer/AuthenticatedIdentities") {}
class SessionAuthCacheKey extends Data.Class {
  [Equal.symbol](that) {
    return this.publicKey === that.publicKey;
  }
  [Hash.symbol]() {
    return Hash.string(this.publicKey);
  }
}
const constEmptyPrivateKey = /*#__PURE__*/Redacted.make(/*#__PURE__*/new Uint8Array(32));
//# sourceMappingURL=EventLogServer.js.map