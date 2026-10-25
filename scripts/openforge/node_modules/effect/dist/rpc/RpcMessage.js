import * as Schema from "../Schema.js";
/**
 * Converts a bigint or string request id into the branded `RequestId` type.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const RequestId = id => id;
/**
 * Represents the reusable `Eof` message value.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const constEof = {
  _tag: "Eof"
};
/**
 * Represents the reusable `Ping` message value.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const constPing = {
  _tag: "Ping"
};
/**
 * The brand identifier used by the `ResponseId` type.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const ResponseIdTypeId = "~effect/rpc/RpcMessage/ResponseId";
/**
 * Creates an encoded terminal response for a request whose exit is a defect.
 *
 * **Details**
 *
 * The defect must already be encoded, because the codec that fills the defect
 * hole belongs to the protocol. Encode it with
 * `protocol.codecFor(Schema.Defect())` first.
 *
 * This constructor produces the structured exit used by JSON-compatible
 * protocols. Serializations whose `codecFor` returns bytes must encode the
 * complete RPC exit before placing it in the response envelope instead.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const ResponseExitDieEncoded = options => ({
  _tag: "Exit",
  requestId: options.requestId,
  exit: {
    _tag: "Failure",
    cause: [{
      _tag: "Die",
      defect: options.encodedDefect
    }]
  }
});
/**
 * Creates a transport-encoded defect response around an already-encoded defect.
 *
 * **Details**
 *
 * Encode the defect with `protocol.codecFor(Schema.Defect())` before wrapping
 * it, because the codec that fills the defect hole belongs to the protocol.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const ResponseDefectEncoded = encodedDefect => ({
  _tag: "Defect",
  defect: encodedDefect
});
const RequestIdSchema = /*#__PURE__*/Schema.Union([Schema.String, Schema.Number]);
// Schema for transport-encoded RPC requests whose payload hole has already
// been filled by the active serialization.
const RequestEncodedSchema = /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Request"),
  id: RequestIdSchema,
  tag: Schema.String,
  payload: Schema.Uint8Array,
  headers: /*#__PURE__*/Schema.Array(/*#__PURE__*/Schema.Tuple([Schema.String, Schema.String])),
  isNotification: /*#__PURE__*/Schema.optional(/*#__PURE__*/Schema.Literal(true)),
  traceId: /*#__PURE__*/Schema.optional(Schema.String),
  spanId: /*#__PURE__*/Schema.optional(Schema.String),
  sampled: /*#__PURE__*/Schema.optional(Schema.Boolean)
});
/**
 * Schema for every transport-encoded RPC envelope that crosses the wire.
 * Binary serializers use it only after each schema-dependent hole has been
 * encoded as bytes. `ClientProtocolError` is excluded because clients create
 * it locally rather than receiving it from a server.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const EncodedSchema = /*#__PURE__*/Schema.Union([RequestEncodedSchema, /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Ack"),
  requestId: RequestIdSchema
}), /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Interrupt"),
  requestId: RequestIdSchema
}), /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Ping")
}), /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Eof")
}), /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Chunk"),
  requestId: RequestIdSchema,
  values: Schema.Uint8Array
}), /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Exit"),
  requestId: RequestIdSchema,
  exit: Schema.Uint8Array
}), /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Defect"),
  defect: Schema.Uint8Array
}), /*#__PURE__*/Schema.Struct({
  _tag: /*#__PURE__*/Schema.tag("Pong")
})]);
/**
 * Represents the reusable `Pong` message value.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const constPong = {
  _tag: "Pong"
};
/**
 * Checks if the response type is terminal.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isTerminalResponse = response => {
  switch (response._tag) {
    case "Exit":
    case "Defect":
    case "ClientProtocolError":
      {
        return true;
      }
    default:
      {
        return false;
      }
  }
};
//# sourceMappingURL=RpcMessage.js.map