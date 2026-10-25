/**
 * Returns the underlying Node `IncomingMessage` for a platform Node
 * `HttpServerRequest`.
 *
 * @category accessors
 * @since 4.0.0
 */
export const toIncomingMessage = self => self.source;
/**
 * Returns the underlying Node `ServerResponse` for a platform Node
 * `HttpServerRequest`, evaluating the stored response thunk when the response
 * was created lazily.
 *
 * @category accessors
 * @since 4.0.0
 */
export const toServerResponse = self => {
  const res = self.response;
  return typeof res === "function" ? res() : res;
};
//# sourceMappingURL=NodeHttpServerRequest.js.map