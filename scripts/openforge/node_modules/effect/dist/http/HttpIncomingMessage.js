import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import { hasProperty } from "../Predicate.js";
import { redact } from "../Redactable.js";
import * as Schema from "../Schema.js";
/**
 * Type identifier for `HttpIncomingMessage` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TypeId = "~effect/http/HttpIncomingMessage";
/**
 * Returns `true` when a value is an `HttpIncomingMessage`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpIncomingMessage = u => hasProperty(u, TypeId);
/**
 * Creates a decoder that reads an incoming message's JSON body and decodes it with the supplied schema.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const schemaBodyJson = (schema, options) => {
  const decode = Schema.decodeEffect(Schema.toCodecJson(schema));
  const decodeJson = options?.reviver === undefined ? undefined : Schema.decodeEffect(Schema.fromJsonString(Schema.toCodecJson(schema), options));
  return self => decodeJson === undefined ? Effect.flatMap(self.json, u => decode(u, options)) : Effect.flatMap(self.text, body => body === "" ? Effect.flatMap(self.json, u => decode(u, options)) : decodeJson(body, options));
};
/**
 * Creates a decoder that reads an incoming message's URL-encoded body parameters and decodes them with the supplied schema.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const schemaBodyUrlParams = (schema, options) => {
  const decode = Schema.RecordFromUrlParams.pipe(Schema.decodeTo(schema), Schema.decodeEffect);
  return self => Effect.flatMap(self.urlParamsBody, u => decode(u, options));
};
/**
 * Creates a decoder that validates and decodes an incoming message's headers with the supplied schema.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const schemaHeaders = (schema, options) => {
  const decode = Schema.decodeUnknownEffect(schema);
  return self => decode(self.headers, options);
};
/**
 * Context reference for the optional maximum size allowed when reading an incoming message body.
 *
 * @stability unstable
 * @category references
 * @since 4.0.0
 */
export const MaxBodySize = /*#__PURE__*/Context.Reference("effect/http/HttpIncomingMessage/MaxBodySize", {
  defaultValue: () => undefined
});
/**
 * Builds an inspectable object for an incoming message, redacting headers and including a synchronously readable JSON or text body when available.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const inspect = (self, that) => {
  const contentType = self.headers["content-type"] ?? "";
  let body;
  if (contentType.includes("application/json")) {
    try {
      body = Effect.runSync(self.json);
      // oxlint-disable-next-line @typescript-eslint/no-unused-vars
    } catch (_) {
      //
    }
  } else if (contentType.includes("text/") || contentType.includes("urlencoded")) {
    try {
      body = Effect.runSync(self.text);
      // oxlint-disable-next-line @typescript-eslint/no-unused-vars
    } catch (_) {
      //
    }
  }
  const obj = {
    ...that,
    headers: redact(self.headers),
    remoteAddress: self.remoteAddress
  };
  if (body !== undefined) {
    obj.body = body;
  }
  return obj;
};
//# sourceMappingURL=HttpIncomingMessage.js.map