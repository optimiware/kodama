import * as Context from "../Context.js";
import * as InternalRecord from "../internal/record.js";
import { pipeArguments } from "../Pipeable.js";
import * as Predicate from "../Predicate.js";
import * as Record from "../Record.js";
const TypeId = "~effect/http-api/HttpApiGroup";
/**
 * Returns `true` when a value is an `HttpApiGroup`, narrowing the value to the
 * group interface.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpApiGroup = u => Predicate.hasProperty(u, TypeId);
const Proto = {
  [TypeId]: TypeId,
  add(...toAdd) {
    const endpoints = {
      ...this.endpoints
    };
    for (const endpoint of toAdd) {
      InternalRecord.assignProperty(endpoints, endpoint.identifier, endpoint);
    }
    return makeProto({
      ...optionsFromGroup(this),
      endpoints
    });
  },
  prefix(prefix) {
    return makeProto({
      ...optionsFromGroup(this),
      endpoints: Record.map(this.endpoints, endpoint => endpoint.prefix(prefix))
    });
  },
  middleware(middleware) {
    return makeProto({
      ...optionsFromGroup(this),
      endpoints: Record.map(this.endpoints, endpoint => endpoint.middleware(middleware))
    });
  },
  annotateMerge(annotations) {
    return makeProto({
      ...optionsFromGroup(this),
      annotations: Context.merge(this.annotations, annotations)
    });
  },
  annotate(annotation, value) {
    return makeProto({
      ...optionsFromGroup(this),
      annotations: Context.add(this.annotations, annotation, value)
    });
  },
  annotateEndpointsMerge(annotations) {
    return makeProto({
      ...optionsFromGroup(this),
      endpoints: Record.map(this.endpoints, endpoint => endpoint.annotateMerge(annotations))
    });
  },
  annotateEndpoints(annotation, value) {
    return makeProto({
      ...optionsFromGroup(this),
      endpoints: Record.map(this.endpoints, endpoint => endpoint.annotate(annotation, value))
    });
  },
  pipe() {
    return pipeArguments(this, arguments);
  }
};
const optionsFromGroup = group => ({
  identifier: group.identifier,
  topLevel: group.topLevel,
  endpoints: group.endpoints,
  annotations: group.annotations
});
const makeProto = options => {
  function HttpApiGroup() {}
  Object.setPrototypeOf(HttpApiGroup, Proto);
  HttpApiGroup.key = `effect/http-api/HttpApiGroup/${options.identifier}`;
  return Object.assign(HttpApiGroup, options);
};
/**
 * Creates an empty `HttpApiGroup` with the supplied identifier.
 *
 * **Details**
 *
 * Add endpoints with `add`, provide implementations with `HttpApiBuilder.group`,
 * and set `topLevel` when the generated client should expose endpoint methods
 * directly instead of nesting them under the group identifier.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = (identifier, options) => makeProto({
  identifier,
  topLevel: options?.topLevel ?? false,
  endpoints: {},
  annotations: Context.empty()
});
//# sourceMappingURL=HttpApiGroup.js.map