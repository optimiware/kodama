/**
 * Defines the message shapes moved through Effect Cluster.
 *
 * Messages carry entity requests and control envelopes between callers, durable
 * storage, transports, and runner handlers. This module includes incoming and
 * outgoing variants for encoded stored requests, decoded local requests,
 * acknowledgements, and interrupts. It also provides helpers for local delivery
 * and for encoding or decoding request payloads with matching RPC schemas.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.js";
import * as Data from "../Data.js";
import * as Effect from "../Effect.js";
import * as Option from "../Option.js";
import * as Rpc from "../rpc/Rpc.js";
import * as Schema from "../Schema.js";
import { MalformedMessage } from "./ClusterError.js";
import * as ClusterSchema from "./ClusterSchema.js";
import * as Envelope from "./Envelope.js";
/**
 * Converts an outgoing message into a locally deliverable incoming message.
 *
 * **Details**
 *
 * Request messages keep their decoded payload and response callback, while
 * control envelopes are wrapped as incoming envelopes.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const incomingLocalFromOutgoing = self => {
  if (self._tag === "OutgoingEnvelope") {
    return new IncomingEnvelope({
      envelope: self.envelope
    });
  }
  return new IncomingRequestLocal({
    annotations: Context.get(self.rpc.annotations, ClusterSchema.Dynamic)(self.rpc.annotations, self.envelope),
    envelope: self.envelope,
    respond: self.respond,
    lastSentReply: Option.none()
  });
};
/**
 * Represents an incoming persisted request whose payload has not yet been decoded with the RPC
 * schema.
 *
 * **Details**
 *
 * It carries the last reply that was sent and a callback for persisting encoded
 * replies.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class IncomingRequest extends /*#__PURE__*/Data.TaggedClass("IncomingRequest") {}
/**
 * Represents an incoming request for local delivery with a decoded payload.
 *
 * **Details**
 *
 * It includes dynamic annotations, the last sent reply, and a callback for
 * replying with decoded replies.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class IncomingRequestLocal extends /*#__PURE__*/Data.TaggedClass("IncomingRequestLocal") {}
/**
 * Represents an incoming control envelope carrying an `AckChunk` or `Interrupt`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class IncomingEnvelope extends /*#__PURE__*/Data.TaggedClass("IncomingEnvelope") {}
/**
 * Represents an outgoing entity request with decoded payload and RPC metadata.
 *
 * **Details**
 *
 * It carries the service context used for serialization, the last received reply,
 * the reply callback, dynamic annotations, and an optional encoded request cache.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class OutgoingRequest extends /*#__PURE__*/Data.TaggedClass("OutgoingRequest") {
  /**
   * Cached encoded envelope payload and the codec that produced it. The cache
   * is reused only when the requested codec is the same function.
   *
   * @stability unstable
   * @since 4.0.0
   */
  encodedCache;
}
/**
 * Represents an outgoing control envelope paired with RPC metadata.
 *
 * **When to use**
 *
 * Use to construct an interrupt envelope for an
 * in-flight request.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class OutgoingEnvelope extends /*#__PURE__*/Data.TaggedClass("OutgoingEnvelope") {
  /**
   * Creates an outgoing interrupt envelope for the supplied request.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static interrupt(options) {
    return new OutgoingEnvelope({
      envelope: new Envelope.Interrupt(options),
      rpc: neverRpc
    });
  }
}
const codecForJson = Schema.toCodecJson;
const neverRpc = /*#__PURE__*/Rpc.make("Never", {
  success: Schema.Never,
  error: Schema.Never,
  payload: {}
});
/**
 * Serializes an outgoing message into a partial envelope.
 *
 * **Details**
 *
 * Control envelopes pass through unchanged. Requests are encoded with their RPC
 * payload schema, reusing the cached encoded request when available.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const serialize = (message, codecFor) => {
  if (message._tag !== "OutgoingRequest") {
    return Effect.succeed(message.envelope);
  }
  return Effect.suspend(() => {
    const cached = message.encodedCache;
    if (cached?.codecFor === codecFor) {
      return Effect.succeed(cached.envelope);
    }
    return Effect.tap(serializeRequest(message, codecFor), envelope => Effect.sync(() => {
      message.encodedCache = {
        codecFor,
        envelope
      };
    }));
  });
};
/**
 * Serializes an outgoing message into its JSON envelope representation.
 *
 * **Details**
 *
 * Schema encoding failures are converted to `MalformedMessage`.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const serializeEnvelope = message => Effect.flatMap(serialize(message, codecForJson), envelope => MalformedMessage.refail(Schema.encodeEffect(Envelope.PartialJson)(envelope)));
/**
 * Encodes the payload of an `OutgoingRequest` with the request's RPC payload
 * schema and service context.
 *
 * **Details**
 *
 * The result is a `PartialRequest` suitable for storage or transport.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const serializeRequest = (self, codecFor) => {
  const rpc = self.rpc;
  return Schema.encodeEffect(codecFor(rpc.payloadSchema))(self.envelope.payload).pipe(Effect.provideContext(self.context), MalformedMessage.refail, Effect.map(payload => ({
    ...self.envelope,
    payload
  })));
};
/**
 * Decodes a partial envelope back into a locally deliverable incoming message.
 *
 * **Details**
 *
 * Control envelopes pass through directly. Request envelopes require the original
 * `OutgoingRequest` so the payload can be decoded with the correct RPC schema and
 * context.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const deserializeLocal = (self, encoded, codecFor) => {
  if (encoded._tag !== "Request") {
    return Effect.succeed(new IncomingEnvelope({
      envelope: encoded
    }));
  } else if (self._tag !== "OutgoingRequest") {
    return Effect.fail(new MalformedMessage({
      cause: new Error("Can only deserialize a Request with an OutgoingRequest message")
    }));
  }
  const rpc = self.rpc;
  return Schema.decodeEffect(codecFor(rpc.payloadSchema))(encoded.payload).pipe(Effect.provideContext(self.context), MalformedMessage.refail, Effect.map(payload => {
    const envelope = Envelope.makeRequest({
      ...encoded,
      payload
    });
    return new IncomingRequestLocal({
      envelope,
      lastSentReply: Option.none(),
      respond: self.respond,
      annotations: Context.get(rpc.annotations, ClusterSchema.Dynamic)(rpc.annotations, envelope)
    });
  }));
};
//# sourceMappingURL=Message.js.map