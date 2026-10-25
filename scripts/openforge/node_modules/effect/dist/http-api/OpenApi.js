/**
 * Generates OpenAPI 3.1 documents from declarative `HttpApi` contracts.
 *
 * The generator reads API groups, endpoints, schemas, security definitions, and
 * annotations, then produces an OpenAPI document. This module also provides the
 * annotations used to shape that output and the TypeScript model for the
 * OpenAPI objects it generates.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Context from "../Context.js";
import * as Equal from "../Equal.js";
import { constFalse } from "../Function.js";
import * as HttpMethod from "../http/HttpMethod.js";
import * as InternalRecord from "../internal/record.js";
import * as InternalToCodec from "../internal/schema/toCodec.js";
import * as InternalToJsonSchemaDocument from "../internal/schema/toJsonSchemaDocument.js";
import * as InternalToRepresentation from "../internal/schema/toRepresentation.js";
import * as JsonSchema from "../JsonSchema.js";
import * as Option from "../Option.js";
import * as Schema from "../Schema.js";
import * as SchemaAST from "../SchemaAST.js";
import * as HttpApi from "./HttpApi.js";
import * as HttpApiEndpoint from "./HttpApiEndpoint.js";
import * as HttpApiMiddleware from "./HttpApiMiddleware.js";
import * as HttpApiSchema from "./HttpApiSchema.js";
import * as HttpApiPath from "./internal/path.js";
/**
 * OpenAPI annotation for overriding generated identifiers, including operation ids.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Identifier extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Identifier") {}
/**
 * OpenAPI annotation for setting the API title or group tag name.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Title extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Title") {}
/**
 * OpenAPI annotation for setting the generated API version.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Version extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Version") {}
/**
 * OpenAPI annotation for setting generated descriptions on APIs, groups, endpoints, or security schemes.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Description extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Description") {}
/**
 * OpenAPI annotation for setting the generated API license metadata.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class License extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/License") {}
/**
 * OpenAPI annotation for adding external documentation metadata to groups or endpoints.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class ExternalDocs extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/ExternalDocs") {}
/**
 * OpenAPI annotation for setting the generated API server list.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Servers extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Servers") {}
/**
 * OpenAPI annotation for setting the format metadata, such as a bearer token format on security schemes.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Format extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Format") {}
/**
 * OpenAPI annotation for setting generated summary text.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Summary extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Summary") {}
/**
 * OpenAPI annotation for marking a generated endpoint operation as deprecated.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Deprecated extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Deprecated") {}
/**
 * OpenAPI annotation for shallowly merging additional fields into a generated OpenAPI object.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Override extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Override") {}
/**
 * Annotation that excludes an annotated group or endpoint from the generated
 * OpenAPI specification.
 *
 * **When to use**
 *
 * Use to hide internal, experimental, or otherwise undocumented HTTP API groups
 * and endpoints from generated OpenAPI output.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export const Exclude = /*#__PURE__*/Context.Reference("effect/http-api/OpenApi/Exclude", {
  defaultValue: constFalse
});
/**
 * OpenAPI annotation for transforming a generated OpenAPI object.
 *
 * **Details**
 *
 * The function is applied during generation to the annotated API, group tag, or
 * endpoint operation.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Transform extends /*#__PURE__*/Context.Service()("effect/http-api/OpenApi/Transform") {}
const servicesPartial = tags => {
  const entries = Object.entries(tags);
  return options => {
    let context = Context.empty();
    for (const [key, tag] of entries) {
      if (options[key] !== undefined) {
        context = Context.add(context, tag, options[key]);
      }
    }
    return context;
  };
};
/**
 * Builds a `Context` containing OpenAPI annotations from the supplied options.
 *
 * @stability unstable
 * @category annotations
 * @since 4.0.0
 */
export const annotations = /*#__PURE__*/servicesPartial({
  identifier: Identifier,
  title: Title,
  version: Version,
  description: Description,
  license: License,
  summary: Summary,
  deprecated: Deprecated,
  externalDocs: ExternalDocs,
  servers: Servers,
  format: Format,
  override: Override,
  exclude: Exclude,
  transform: Transform
});
const defaultOptions = {};
const apiCache = /*#__PURE__*/new WeakMap();
const cloneOpenAPISpec = value => {
  if (Array.isArray(value)) {
    return value.map(cloneOpenAPISpec);
  }
  if (value !== null && typeof value === "object" && !Object.isFrozen(value)) {
    const out = {};
    for (const key of Object.keys(value)) {
      InternalRecord.assignProperty(out, key, cloneOpenAPISpec(value[key]));
    }
    return out;
  }
  return value;
};
/**
 * This function checks if a given tag exists within the provided context. If
 * the tag is present, it retrieves the associated value and applies the given
 * callback function to it. If the tag is not found, the function does nothing.
 */
function processAnnotation(ctx, annotation, f) {
  const o = Context.getOption(ctx, annotation);
  if (Option.isSome(o)) {
    f(o.value);
  }
}
/**
 * Generates an OpenAPI 3.1 specification from an `HttpApi`.
 *
 * **When to use**
 *
 * Use when you need a programmatic OpenAPI document for an API definition.
 *
 * **Details**
 *
 * This function takes an `HttpApi` instance, which defines a structured API,
 * and generates an OpenAPI Specification (`OpenAPISpec`). The resulting spec
 * adheres to the OpenAPI 3.1.0 standard and includes detailed metadata such as
 * paths, operations, security schemes, and components. The function processes
 * the API's annotations, middleware, groups, and endpoints to build a complete
 * and accurate representation of the API in OpenAPI format.
 *
 * The function also deduplicates schemas, applies transformations, and
 * integrates annotations like descriptions, summaries, external documentation,
 * and overrides. The optional reference policy receives canonical JSON encoded
 * ASTs and controls which schemas are extracted into components. By default, only candidates with resolved identifiers
 * become references; anonymous non-recursive schemas remain inline.
 *
 * **Gotchas**
 *
 * Cached results are keyed by both the `HttpApi` instance and the identity of the options object. Reuse the same immutable
 * options object to reuse a cached result; mutating an options object after its first use does not invalidate the cached
 * specification. Each call returns a copy of the cached specification.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export function fromApi(api, options) {
  const resolvedOptions = options ?? defaultOptions;
  let cache = apiCache.get(resolvedOptions);
  if (cache === undefined) {
    cache = new WeakMap();
    apiCache.set(resolvedOptions, cache);
  }
  const cached = cache.get(api);
  if (cached !== undefined) return cloneOpenAPISpec(cached);
  const spec = makeOpenApi(api, resolvedOptions);
  cache.set(api, cloneOpenAPISpec(spec));
  return spec;
}
function makeOpenApi(api, options) {
  let spec = {
    openapi: "3.1.0",
    info: {
      title: "Api",
      version: "0.0.1"
    },
    paths: {},
    components: {
      schemas: {},
      securitySchemes: {}
    },
    security: [],
    tags: []
  };
  const schemaOps = [];
  const pathOperations = new Set();
  const operationIds = new Set();
  const finalizeOperations = [];
  function registerSchema(ast, encoding = "Json") {
    // Text bodies remain strings even when an opaque schema compiles to {}.
    const target = encoding === "Text" ? {
      type: "string"
    } : {};
    schemaOps.push({
      _tag: "schema",
      ast: toEncodingAST(ast, encoding),
      target
    });
    return target;
  }
  processAnnotation(api.annotations, Title, title => {
    spec.info.title = title;
  });
  processAnnotation(api.annotations, Version, version => {
    spec.info.version = version;
  });
  processAnnotation(api.annotations, Description, description => {
    spec.info.description = description;
  });
  processAnnotation(api.annotations, License, license => {
    spec.info.license = license;
  });
  processAnnotation(api.annotations, Summary, summary => {
    spec.info.summary = summary;
  });
  processAnnotation(api.annotations, Servers, servers => {
    spec.servers = [...servers];
  });
  HttpApi.reflect(api, {
    onGroup({
      group
    }) {
      if (Context.get(group.annotations, Exclude)) {
        return;
      }
      let tag = {
        name: Context.getOrElse(group.annotations, Title, () => group.identifier)
      };
      processAnnotation(group.annotations, Description, description => {
        tag.description = description;
      });
      processAnnotation(group.annotations, ExternalDocs, externalDocs => {
        tag.externalDocs = externalDocs;
      });
      processAnnotation(group.annotations, Override, override => {
        // OpenAPI documents are JSON, so symbol keys are intentionally ignored.
        for (const [key, value] of Object.entries(override)) {
          InternalRecord.assignProperty(tag, key, value);
        }
      });
      processAnnotation(group.annotations, Transform, transformFn => {
        tag = transformFn(tag);
      });
      spec.tags.push(tag);
    },
    onEndpoint({
      endpoint,
      group,
      mergedAnnotations,
      middleware
    }) {
      if (Context.get(mergedAnnotations, Exclude)) {
        return;
      }
      const op = {
        tags: [Context.getOrElse(group.annotations, Title, () => group.identifier)],
        operationId: Context.getOrElse(endpoint.annotations, Identifier, () => group.topLevel ? endpoint.identifier : `${group.identifier}.${endpoint.identifier}`),
        parameters: [],
        security: [],
        responses: {}
      };
      const paramNames = HttpApiPath.getParamNames(endpoint.params);
      const path = endpoint.path.replace(/:(\w+)\??/g, (match, key) => paramNames === undefined || paramNames.has(key) ? `{${key}}` : match);
      const method = endpoint.method.toLowerCase();
      const isQuery = method === "query";
      const operationKey = isQuery ? "QUERY" : method;
      function processResponseBodies(bodies, defaultDescription) {
        for (const [status, {
          content,
          descriptions,
          headers,
          streamContent
        }] of bodies) {
          const description = descriptions.size > 0 ? Array.from(descriptions).join(" | ") : defaultDescription();
          InternalRecord.assignProperty(op.responses, status, {
            description
          });
          for (const schema of headers) {
            schemaOps.push({
              _tag: "properties",
              ast: schema.ast,
              select: property => String(property.name).toLowerCase() !== "content-type",
              apply(property, schema) {
                const name = String(property.name).toLowerCase();
                op.responses[status].headers ??= {};
                InternalRecord.assignProperty(op.responses[status].headers, name, {
                  schema,
                  required: !property.isOptional
                });
              }
            });
          }
          if (content !== undefined) {
            content.forEach((map, encoding) => {
              map.forEach((schemas, contentType) => {
                const asts = Array.from(schemas, SchemaAST.getAST);
                const ast = asts.length === 1 ? asts[0] : new SchemaAST.Union(asts);
                op.responses[status].content ??= {};
                InternalRecord.assignProperty(op.responses[status].content, contentType, {
                  schema: registerSchema(ast, encoding)
                });
              });
            });
          }
          if (streamContent !== undefined) {
            streamContent.forEach((stream, contentType) => {
              op.responses[status].content ??= {};
              if (HttpApiSchema.isStreamSse(stream)) {
                InternalRecord.assignProperty(op.responses[status].content, contentType, {
                  schema: registerSchema(stream.events.ast),
                  "x-effect-stream": {
                    encoding: "sse",
                    causeSchema: registerSchema(Schema.Cause(stream.error, Schema.Defect()).ast),
                    errorSchema: registerSchema(stream.error.ast),
                    failureEvent: reservedStreamFailureEvent
                  }
                });
              } else {
                InternalRecord.assignProperty(op.responses[status].content, contentType, {
                  schema: {
                    type: "string",
                    format: "binary"
                  },
                  "x-effect-stream": {
                    encoding: "uint8array"
                  }
                });
              }
            });
          }
        }
      }
      function processParameters(schema, i) {
        if (schema) {
          schemaOps.push({
            _tag: "properties",
            ast: schema.ast,
            apply(property, schema) {
              op.parameters.push({
                name: String(property.name),
                in: i,
                schema,
                required: i === "path" || !property.isOptional
              });
            }
          });
        }
      }
      processAnnotation(endpoint.annotations, Description, description => {
        op.description = description;
      });
      processAnnotation(endpoint.annotations, Summary, summary => {
        op.summary = summary;
      });
      processAnnotation(endpoint.annotations, Deprecated, deprecated => {
        op.deprecated = deprecated;
      });
      processAnnotation(endpoint.annotations, ExternalDocs, externalDocs => {
        op.externalDocs = externalDocs;
      });
      middleware.forEach(middleware => {
        if (!HttpApiMiddleware.isSecurity(middleware)) {
          return;
        }
        for (const [name, security] of Object.entries(middleware.security)) {
          processHttpApiSecurity(name, security);
          op.security.push({
            [name]: []
          });
        }
      });
      function processHttpApiSecurity(name, security) {
        const scheme = makeSecurityScheme(security);
        if (!Object.hasOwn(spec.components.securitySchemes, name)) {
          InternalRecord.assignProperty(spec.components.securitySchemes, name, scheme);
          return;
        }
        if (!Equal.equals(securitySchemeForComparison(spec.components.securitySchemes[name]), securitySchemeForComparison(scheme))) {
          throw new globalThis.Error(`Conflicting OpenAPI security scheme: ${name}`);
        }
      }
      const hasBody = HttpMethod.hasBody(endpoint.method);
      if (hasBody) {
        const schemasByContentType = new Map();
        for (const schema of HttpApiEndpoint.getPayloadSchemas(endpoint)) {
          if (HttpApiSchema.isNoContent(schema.ast)) continue;
          const encoding = HttpApiSchema.getPayloadEncoding(schema.ast, endpoint.method);
          const existing = schemasByContentType.get(encoding.contentType);
          if (existing === undefined) {
            schemasByContentType.set(encoding.contentType, {
              encoding,
              schemas: [schema]
            });
          } else {
            existing.schemas.push(schema);
          }
        }
        if (schemasByContentType.size > 0) {
          const content = {};
          for (const [contentType, {
            encoding,
            schemas
          }] of schemasByContentType) {
            const asts = schemas.map(SchemaAST.getAST);
            const ast = asts.length === 1 ? asts[0] : new SchemaAST.Union(asts);
            InternalRecord.assignProperty(content, contentType, {
              schema: registerSchema(ast, encoding._tag)
            });
          }
          op.requestBody = {
            content,
            required: true
          };
        }
      }
      processParameters(endpoint.params, "path");
      if (!hasBody && endpoint.payload.size === 1) {
        const entry = endpoint.payload.values().next().value;
        processParameters(entry.schemas[0], "query");
      }
      processParameters(endpoint.headers, "header");
      processParameters(endpoint.query, "query");
      processResponseBodies(extractSuccessResponseBodies(endpoint), () => "Success");
      processResponseBodies(extractResponseBodies(HttpApiEndpoint.getErrorSchemas(endpoint), HttpApiSchema.getStatusErrorSchema, resolveDescriptionOrIdentifier), () => "Error");
      const pathOperation = `${method} ${path.replace(/\{[^}]+\}/g, "{}")}`;
      if (pathOperations.has(pathOperation)) {
        throw new globalThis.Error(`Duplicate OpenAPI operation for ${endpoint.method} ${path}`);
      }
      pathOperations.add(pathOperation);
      if (!Object.hasOwn(spec.paths, path)) {
        InternalRecord.assignProperty(spec.paths, path, {});
      }
      const getOperations = () => {
        const pathItem = spec.paths[path];
        return isQuery ? pathItem["x-oai-additionalOperations"] ??= {} : pathItem;
      };
      getOperations()[operationKey] = op;
      finalizeOperations.push(() => {
        const operations = getOperations();
        let op = operations[operationKey];
        processAnnotation(endpoint.annotations, Override, override => {
          // OpenAPI documents are JSON, so symbol keys are intentionally ignored.
          for (const [key, value] of Object.entries(override)) {
            InternalRecord.assignProperty(op, key, value);
          }
        });
        processAnnotation(endpoint.annotations, Transform, transformFn => {
          op = transformFn(op);
        });
        const operationId = op.operationId;
        if (operationId !== undefined) {
          if (operationIds.has(operationId)) {
            throw new globalThis.Error(`Duplicate OpenAPI operationId: ${operationId}`);
          }
          operationIds.add(operationId);
        }
        operations[operationKey] = op;
      });
    }
  });
  processAnnotation(api.annotations, HttpApi.AdditionalSchemas, componentSchemas => {
    componentSchemas.forEach(componentSchema => {
      const identifier = SchemaAST.resolveIdentifier(componentSchema.ast);
      if (identifier !== undefined) {
        if (Object.hasOwn(spec.components.schemas, identifier)) {
          throw new globalThis.Error(`Duplicate component schema identifier: ${identifier}`);
        }
        InternalRecord.assignProperty(spec.components.schemas, identifier, registerSchema(componentSchema.ast));
      }
    });
  });
  compileSchemaOps(schemaOps, options, spec.components.schemas);
  for (const finalize of finalizeOperations) {
    finalize();
  }
  Object.keys(spec.components.schemas).forEach(key => {
    if (!JsonSchema.VALID_OPEN_API_COMPONENTS_SCHEMAS_KEY_REGEXP.test(key)) {
      throw new globalThis.Error(`Invalid component schema key: ${key}`);
    }
  });
  processAnnotation(api.annotations, Override, override => {
    // OpenAPI documents are JSON, so symbol keys are intentionally ignored.
    for (const [key, value] of Object.entries(override)) {
      InternalRecord.assignProperty(spec, key, value);
    }
  });
  processAnnotation(api.annotations, Transform, transformFn => {
    spec = transformFn(spec);
  });
  return spec;
}
function compileSchemaOps(schemaOps, options, components) {
  if (!Arr.isArrayNonEmpty(schemaOps)) return;
  const document = InternalToRepresentation.toRepresentations(Arr.map(schemaOps, op => op.ast), options);
  const representations = Arr.map(document.representations, (representation, index) => {
    const op = schemaOps[index];
    return op._tag === "schema" ? representation : makePropertiesRepresentation(representation, document.references, op.select);
  });
  const references = selectReferences(representations, document.references);
  const jsonSchemaMultiDocument = JsonSchema.toMultiDocumentOpenApi3_1(InternalToJsonSchemaDocument.toJsonSchemaMultiDocument({
    representations,
    references
  }, {
    onExcessProperty: "error"
  }));
  for (let index = 0; index < schemaOps.length; index++) {
    const op = schemaOps[index];
    const schema = jsonSchemaMultiDocument.schemas[index];
    if (op._tag === "schema") {
      InternalRecord.assignProperties(op.target, schema);
      continue;
    }
    const representation = representations[index];
    if (representation._tag !== "Objects") {
      throw new globalThis.Error("Expected an object representation for HTTP properties");
    }
    // These schemas come from the synthetic Objects below, without root checks or annotations.
    const properties = schema.properties;
    for (const property of representation.propertySignatures) {
      op.apply(property, properties[String(property.name)]);
    }
  }
  for (const [name, definition] of Object.entries(jsonSchemaMultiDocument.definitions)) {
    InternalRecord.assignProperty(components, name, definition);
  }
}
function makePropertiesRepresentation(representation, references, select) {
  const resolved = resolveRepresentation(representation, references);
  return {
    _tag: "Objects",
    propertySignatures: resolved._tag === "Objects" ? resolved.propertySignatures.filter(property => select?.(property) ?? true).map(property => ({
      ...property,
      name: String(property.name)
    })) : [],
    indexSignatures: [],
    checks: []
  };
}
function resolveRepresentation(representation, references, seen = new Set()) {
  if (representation._tag === "Suspend") {
    return resolveRepresentation(representation.thunk, references, seen);
  }
  if (representation._tag !== "Reference" || seen.has(representation.$ref)) return representation;
  const target = references[representation.$ref];
  return target === undefined ? representation : resolveRepresentation(target, references, new Set(seen).add(representation.$ref));
}
function selectReferences(representations, references) {
  // The JSON Schema compiler emits every supplied reference. Keep only references reachable from the projected
  // roots so named parameter containers and excluded properties do not leak into OpenAPI components.
  const selected = {};
  const visited = new WeakSet();
  const stack = [...representations];
  function pushChecks(checks) {
    for (const check of checks) {
      if (check.representation?.schemas !== undefined) stack.push(...check.representation.schemas);
      if (check._tag === "FilterGroup") pushChecks(check.checks);
    }
  }
  while (stack.length > 0) {
    const representation = stack.pop();
    if (visited.has(representation)) continue;
    visited.add(representation);
    if (representation._tag === "Reference") {
      if (Object.hasOwn(selected, representation.$ref)) continue;
      const target = references[representation.$ref];
      if (target === undefined) continue;
      InternalRecord.assignProperty(selected, representation.$ref, target);
      stack.push(target);
      continue;
    }
    pushChecks(representation.checks);
    switch (representation._tag) {
      case "Declaration":
        stack.push(...representation.typeParameters);
        break;
      case "Suspend":
        stack.push(representation.thunk);
        break;
      case "TemplateLiteral":
        stack.push(...representation.parts);
        break;
      case "Arrays":
        for (const element of representation.elements) stack.push(element.type);
        stack.push(...representation.rest);
        break;
      case "Objects":
        for (const property of representation.propertySignatures) stack.push(property.type);
        for (const signature of representation.indexSignatures) {
          stack.push(signature.parameter, signature.type);
        }
        break;
      case "Union":
        stack.push(...representation.types);
        break;
    }
  }
  return selected;
}
const reservedStreamFailureEvent = "effect/http-api/stream/failure";
function extractSuccessResponseBodies(endpoint) {
  return extractResponseBodies(HttpApiEndpoint.getSuccessSchemas(endpoint), HttpApiSchema.getStatusSuccessSchema, resolveDescriptionOrIdentifier);
}
function extractResponseBodies(schemas, getStatus, getDescription) {
  const map = new Map();
  schemas.forEach(process);
  return map;
  function process(schema) {
    const annotation = HttpApiSchema.getWithHeadersAnnotation(schema.ast);
    const body = HttpApiSchema.isWithHeaders(schema) ? schema.schema : annotation?.body ?? schema;
    const headers = HttpApiSchema.isWithHeaders(schema) ? schema.headers : annotation?.headersCodec;
    const status = getStatus(schema);
    const ast = body.ast;
    if (HttpApiSchema.isStreamSchema(body)) {
      addStreamContent(body, status);
    } else if (HttpApiSchema.isNoContent(ast)) {
      addNoContent(status, getDescription(schema.ast) ?? getDescription(ast) ?? "<No Content>");
    } else {
      addContent(body, status, HttpApiSchema.getResponseEncodingSchema(schema), getDescription(schema.ast) ?? getDescription(ast));
    }
    if (headers !== undefined) {
      map.get(status).headers.push(headers);
    }
  }
  function addNoContent(status, description) {
    const statusMap = map.get(status);
    if (statusMap === undefined) {
      map.set(status, {
        descriptions: new Set([description]),
        content: undefined,
        headers: [],
        streamContent: undefined
      });
    } else {
      if (description !== undefined) {
        statusMap.descriptions.add(description);
      }
    }
  }
  function addContent(schema, status, encoding, description) {
    const statusMap = map.get(status);
    const {
      _tag,
      contentType
    } = encoding;
    if (statusMap === undefined) {
      map.set(status, {
        descriptions: new Set(description !== undefined ? [description] : []),
        content: new Map([[_tag, new Map([[contentType, new Set([schema])]])]]),
        headers: [],
        streamContent: undefined
      });
    } else {
      // concat descriptions
      if (description !== undefined) {
        statusMap.descriptions.add(description);
      }
      if (statusMap.content === undefined) {
        statusMap.content = new Map([[_tag, new Map([[contentType, new Set([schema])]])]]);
      } else {
        const schemasByContentType = statusMap.content.get(_tag);
        if (schemasByContentType === undefined) {
          statusMap.content.set(_tag, new Map([[contentType, new Set([schema])]]));
        } else {
          const set = schemasByContentType.get(contentType);
          if (set === undefined) {
            schemasByContentType.set(contentType, new Set([schema]));
          } else {
            set.add(schema);
          }
        }
      }
    }
  }
  function addStreamContent(stream, status) {
    const statusMap = map.get(status);
    if (statusMap === undefined) {
      map.set(status, {
        descriptions: new Set(),
        content: undefined,
        headers: [],
        streamContent: new Map([[stream.contentType, stream]])
      });
    } else {
      if (statusMap.streamContent === undefined) {
        statusMap.streamContent = new Map([[stream.contentType, stream]]);
      } else {
        statusMap.streamContent.set(stream.contentType, stream);
      }
    }
  }
}
function resolveDescriptionOrIdentifier(ast) {
  return SchemaAST.resolveDescription(ast) ?? SchemaAST.resolveIdentifier(ast);
}
const Uint8ArrayEncoding = /*#__PURE__*/Schema.String.annotate({
  format: "binary"
});
function toEncodingAST(ast, _tag) {
  switch (_tag) {
    case "Uint8Array":
      return Uint8ArrayEncoding.ast;
    case "Text":
      return SchemaAST.isDeclaration(ast) && ast.encoding === undefined ? Schema.String.ast : ast;
    case "Multipart":
      ast = persistedFileToBinaryEncoding(ast);
  }
  return InternalToCodec.toCodecJsonAST(ast);
}
function persistedFileToBinaryEncoding(ast) {
  if (SchemaAST.isDeclaration(ast) && ast.annotations?.representation?.id === "effect/http/PersistedFile") {
    return Uint8ArrayEncoding.ast;
  }
  if (typeof ast?.recur === "function") {
    return ast.recur(persistedFileToBinaryEncoding);
  }
  return ast;
}
const makeSecurityScheme = security => {
  const meta = {};
  processAnnotation(security.annotations, Description, description => {
    meta.description = description;
  });
  switch (security._tag) {
    case "Basic":
      {
        return {
          ...meta,
          type: "http",
          scheme: "basic"
        };
      }
    case "Http":
      {
        const format = Context.getOption(security.annotations, Format).pipe(Option.map(format => ({
          bearerFormat: format
        })), Option.getOrUndefined);
        return {
          ...meta,
          type: "http",
          scheme: security.scheme,
          ...format
        };
      }
    case "ApiKey":
      {
        return {
          ...meta,
          type: "apiKey",
          name: security.key,
          in: security.in
        };
      }
  }
};
const securitySchemeForComparison = scheme => {
  if (scheme.type === "http") {
    return {
      ...scheme,
      scheme: scheme.scheme.toLowerCase()
    };
  }
  if (scheme.in === "header") {
    return {
      ...scheme,
      name: scheme.name.toLowerCase()
    };
  }
  return scheme;
};
//# sourceMappingURL=OpenApi.js.map