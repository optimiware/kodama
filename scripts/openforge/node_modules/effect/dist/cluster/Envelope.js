import * as Predicate from "../Predicate.js";
import * as PrimaryKey from "../PrimaryKey.js";
import * as Schema from "../Schema.js";
import * as SchemaTransformation from "../SchemaTransformation.js";
import { EntityAddress } from "./EntityAddress.js";
import { SnowflakeFromBigInt } from "./Snowflake.js";
/**
 * Type identifier used to mark runtime cluster envelope values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TypeId = "~effect/cluster/Envelope";
/**
 * Schema for a value that has already been encoded by the transport's hole
 * codec.
 *
 * **Details**
 *
 * Cluster payloads are encoded twice: the entity payload is encoded with the
 * entity RPC schema, and the result is carried opaquely inside the runner
 * envelope. This schema names that hole so the outer runner encode leaves it
 * alone. It is the identity under `Schema.toCodecJson`, so JSON, NDJSON, and
 * text transports stay wire-compatible. A binary codec compiles it as a
 * bytes leaf.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const OpaqueHole = /*#__PURE__*/Schema.declare(_ => true, {
  expected: "an already-encoded value",
  toCodecJson: () => undefined,
  toCodec: () => Schema.link()(Schema.Uint8Array, SchemaTransformation.passthrough())
});
/**
 * Schema for a request envelope before its RPC payload has been decoded.
 *
 * **Details**
 *
 * The envelope metadata is decoded, while the payload remains `unknown` until it
 * is decoded with the target RPC payload schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class PartialRequest extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Request"),
  requestId: SnowflakeFromBigInt,
  address: EntityAddress,
  tag: Schema.String,
  payload: OpaqueHole,
  headers: Schema.Headers,
  traceId: /*#__PURE__*/Schema.optional(Schema.String),
  spanId: /*#__PURE__*/Schema.optional(Schema.String),
  sampled: /*#__PURE__*/Schema.optional(Schema.Boolean)
})) {}
/**
 * Represents an envelope acknowledging receipt of a streamed reply chunk for a
 * request.
 *
 * **Details**
 *
 * The `replyId` identifies the chunk reply that has been received.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class AckChunk extends /*#__PURE__*/Schema.Class("effect/cluster/Envelope/AckChunk")({
  _tag: /*#__PURE__*/Schema.tag("AckChunk"),
  id: SnowflakeFromBigInt,
  address: EntityAddress,
  requestId: SnowflakeFromBigInt,
  replyId: SnowflakeFromBigInt
}) {
  /**
   * Marks this value as a cluster envelope for runtime guards.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [TypeId] = TypeId;
  /**
   * Returns a copy of this acknowledgement associated with the supplied request id.
   *
   * @stability unstable
   * @since 4.0.0
   */
  withRequestId(requestId) {
    return new AckChunk({
      ...this,
      requestId
    });
  }
}
/**
 * Represents an envelope used to interrupt an in-flight entity request.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class Interrupt extends /*#__PURE__*/Schema.Class("effect/cluster/Envelope/Interrupt")({
  _tag: /*#__PURE__*/Schema.tag("Interrupt"),
  id: SnowflakeFromBigInt,
  address: EntityAddress,
  requestId: SnowflakeFromBigInt
}) {
  /**
   * Marks this value as a cluster envelope for runtime guards.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [TypeId] = TypeId;
  /**
   * Returns a copy of this interrupt associated with the supplied request id.
   *
   * @stability unstable
   * @since 4.0.0
   */
  withRequestId(requestId) {
    return new Interrupt({
      ...this,
      requestId
    });
  }
}
/**
 * Schema for partially decoded cluster envelopes.
 *
 * **Details**
 *
 * It accepts `PartialRequest`, `AckChunk`, and `Interrupt` envelope values.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Partial = /*#__PURE__*/Schema.Union([PartialRequest, AckChunk, Interrupt]);
/**
 * JSON codec for partial cluster envelopes.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const PartialJson = /*#__PURE__*/Schema.toCodecJson(Partial);
/**
 * Schema for mutable arrays of JSON-encoded partial cluster envelopes.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const PartialArray = /*#__PURE__*/Schema.mutable(/*#__PURE__*/Schema.Array(PartialJson));
/**
 * Returns `true` when the supplied value is a runtime cluster envelope.
 *
 * **Details**
 *
 * The check is based on the envelope type identifier.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isEnvelope = u => Predicate.hasProperty(u, TypeId);
/**
 * Constructs a runtime request envelope and attaches the envelope type identifier.
 *
 * **Details**
 *
 * Tracing fields are included only when a `traceId` is provided.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeRequest = options => ({
  [TypeId]: TypeId,
  _tag: "Request",
  requestId: options.requestId,
  tag: options.tag,
  address: options.address,
  payload: options.payload,
  headers: options.headers,
  ...(options.traceId !== undefined ? {
    traceId: options.traceId,
    spanId: options.spanId,
    sampled: options.sampled
  } : {})
});
/**
 * Schema for runtime cluster envelopes recognized by their type identifier.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const Envelope = /*#__PURE__*/Schema.declare(isEnvelope, {
  identifier: "Envelope"
});
/**
 * Schema for runtime request envelopes.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const Request = /*#__PURE__*/Schema.declare(u => isEnvelope(u) && u._tag === "Request", {
  identifier: "Request"
});
/**
 * Transforms plain request data with `makeRequest` and encodes
 * request envelopes back to their raw representation.
 *
 * @stability unstable
 * @category serialization
 * @since 4.0.0
 */
export const RequestTransform = /*#__PURE__*/SchemaTransformation.transform({
  decode: u => makeRequest(u),
  encode: u => u
});
/**
 * Returns the storage primary key for a request envelope whose payload has a
 * primary key, or `null` when the envelope is not a keyed request.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const primaryKey = envelope => {
  if (envelope._tag !== "Request" || !PrimaryKey.isPrimaryKey(envelope.payload)) {
    return null;
  }
  return primaryKeyByAddress({
    address: envelope.address,
    tag: envelope.tag,
    id: PrimaryKey.value(envelope.payload)
  });
};
/**
 * Builds a storage primary-key string from an entity address, RPC tag, and
 * payload primary-key ID.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const primaryKeyByAddress = options =>
// storage drivers with fixed-width key columns (e.g. SqlMessageStorage)
// hash this composed key at their own boundary
`${options.address.entityType}/${options.address.entityId}/${options.tag}/${options.id}`;
//# sourceMappingURL=Envelope.js.map