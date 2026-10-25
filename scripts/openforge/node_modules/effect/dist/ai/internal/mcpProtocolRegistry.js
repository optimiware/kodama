import * as Cause from "../../Cause.js";
import * as Effect from "../../Effect.js";
const prefix = protocol => `@effect/mcp/${encodeURIComponent(protocol.protocolVersion)}/`;
// NOTE: Protocol selection and request namespacing happen before an adapter's
// payload codec decodes the request. Canonical McpSchema value reuse must not
// introduce a shared permissive decode-first path.
/**
 * @internal
 */
export const make = /*#__PURE__*/Effect.fnUntraced(function* (protocols) {
  if (protocols.length === 0) {
    return yield* new Cause.IllegalArgumentError("MCP protocol declaration must contain at least one MCP protocol");
  }
  const snapshot = Object.freeze(Array.from(protocols));
  const byVersion = new Map();
  for (const protocol of snapshot) {
    if (byVersion.has(protocol.protocolVersion)) {
      return yield* new Cause.IllegalArgumentError(`Duplicate MCP protocol version: ${protocol.protocolVersion}`);
    }
    byVersion.set(protocol.protocolVersion, protocol);
  }
  let clientRpcs = snapshot[0].clientRpcs.prefix(prefix(snapshot[0]));
  for (let i = 1; i < snapshot.length; i++) {
    clientRpcs = clientRpcs.merge(snapshot[i].clientRpcs.prefix(prefix(snapshot[i])));
  }
  return {
    protocols: snapshot,
    clientRpcs,
    select: offeredVersion => byVersion.get(offeredVersion) ?? snapshot[0],
    protocolForInternalTag: tag => snapshot.find(protocol => tag.startsWith(prefix(protocol))) ?? snapshot[0],
    routeClientRequest: (protocol, request) => ({
      ...request,
      tag: `${prefix(protocol)}${request.tag}`
    }),
    handlerTarget: (contextMap, context) => ({
      context,
      install: Effect.fnUntraced(function* (protocol, rpcs, handlers) {
        const handlerContext = yield* rpcs.toHandlers(handlers);
        for (const rpcDefinition of rpcs.requests.values()) {
          const namespacedRpc = clientRpcs.requests.get(`${prefix(protocol)}${rpcDefinition._tag}`);
          const handler = handlerContext.mapUnsafe.get(rpcDefinition.key);
          if (namespacedRpc === undefined || handler === undefined) {
            return yield* Effect.die(`MCP handler registration invariant failed for ${protocol.protocolVersion}/${rpcDefinition._tag}`);
          }
          contextMap.set(namespacedRpc.key, handler);
        }
      })
    })
  };
});
//# sourceMappingURL=mcpProtocolRegistry.js.map