/**
 * Defines schemas for Model Context Protocol messages.
 *
 * MCP clients and servers use these schemas to describe the JSON-RPC requests,
 * notifications, results, and errors that can cross the protocol boundary.
 * This is the stable public compatibility and authoring surface. It is not an
 * exact dated wire contract: MCP protocol adapters use frozen schemas under
 * `internal/mcpSchema` for decoding and encoding. This module groups the
 * current public message model for application authors and provides helpers
 * for optional fields and parameter metadata. Transport and server behavior
 * live in other modules.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Predicate from "../Predicate.ts";
import * as Rpc from "../rpc/Rpc.ts";
import * as RpcGroup from "../rpc/RpcGroup.ts";
import * as RpcMiddleware from "../rpc/RpcMiddleware.ts";
import * as Schema from "../Schema.ts";
import type * as Scope from "../Scope.ts";
import type { ProtocolVersion, StatefulProtocolVersion } from "./McpProtocol.ts";
/**
 * Schema type returned by `optionalWithDefault`.
 *
 * **Details**
 *
 * It represents an optional struct field that supplies a default value when the
 * field is absent during decoding or construction.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface optionalWithDefault<S extends Schema.Constraint & Schema.WithoutConstructorDefault> extends Schema.withConstructorDefault<Schema.decodeTo<Schema.toType<Schema.optionalKey<S>>, Schema.optionalKey<S>>> {
}
/**
 * Marks a struct field as optional and supplies `defaultValue` when the field
 * is absent.
 *
 * **Details**
 *
 * The default is used during decoding and as the constructor default for the
 * schema field.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const optionalWithDefault: <S extends Schema.Constraint & Schema.WithoutConstructorDefault>(schema: S, defaultValue: () => Schema.optionalKey<S>["Type"]) => optionalWithDefault<S>;
/**
 * Creates an optional MCP struct-field schema from a required schema.
 *
 * **Details**
 *
 * The field may be absent, and explicit `undefined` values are omitted when
 * encoding.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const optional: <S extends Schema.Constraint>(schema: S) => Schema.decodeTo<Schema.toType<Schema.optional<S>>, Schema.optionalKey<S>>;
/**
 * Schema for JSON-RPC request identifiers, allowing string or number ids.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const RequestId: Schema.Union<[
    typeof Schema.String,
    typeof Schema.Finite
]>;
/**
 * Type represented by the JSON-RPC request identifier schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type RequestId = typeof RequestId.Type;
/**
 * Schema for MCP progress tokens that associate progress notifications with the
 * original request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const ProgressToken: Schema.Union<[
    typeof Schema.String,
    typeof Schema.Finite
]>;
/**
 * Type represented by the MCP progress token schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ProgressToken = typeof ProgressToken.Type;
declare const RequestMeta_base: Schema.Opaque<RequestMeta, Schema.Struct<{
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for optional MCP request metadata.
 *
 * **Details**
 *
 * Request metadata may include a progress token that asks the receiver to send
 * out-of-band progress notifications for the request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class RequestMeta extends RequestMeta_base {
}
declare const ResultMeta_base: Schema.Opaque<ResultMeta, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for optional MCP result metadata.
 *
 * **Details**
 *
 * The `_meta` field is reserved for protocol, extension, or implementation
 * metadata attached to a result.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ResultMeta extends ResultMeta_base {
}
declare const NotificationMeta_base: Schema.Opaque<NotificationMeta, Schema.Struct<{
    /**
     * This parameter name is reserved by MCP to allow clients and servers to
     * attach additional metadata to their notifications.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * This parameter name is reserved by MCP to allow clients and servers to
     * attach additional metadata to their notifications.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for optional MCP notification metadata.
 *
 * **Details**
 *
 * The `_meta` field is reserved for protocol, extension, or implementation
 * metadata attached to a notification.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class NotificationMeta extends NotificationMeta_base {
}
/**
 * Schema for opaque cursor tokens used in pagination.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const Cursor: typeof Schema.String;
/**
 * Type represented by the MCP cursor schema.
 *
 * **Details**
 *
 * A cursor is an opaque string token used to continue paginated requests.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Cursor = typeof Cursor.Type;
declare const PaginatedRequestMeta_base: Schema.Opaque<PaginatedRequestMeta, Schema.Struct<{
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * An opaque token representing the current pagination position.
     * If provided, the server should return results starting after this cursor.
     */
    readonly cursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * An opaque token representing the current pagination position.
     * If provided, the server should return results starting after this cursor.
     */
    readonly cursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for MCP request metadata used by paginated requests.
 *
 * **Details**
 *
 * It includes the base request metadata fields plus an optional cursor
 * indicating where the server should continue listing results.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class PaginatedRequestMeta extends PaginatedRequestMeta_base {
}
declare const PaginatedResultMeta_base: Schema.Opaque<PaginatedResultMeta, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * An opaque token representing the pagination position after the last returned result.
     * If present, there may be more results available.
     */
    readonly nextCursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * An opaque token representing the pagination position after the last returned result.
     * If present, there may be more results available.
     */
    readonly nextCursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for MCP result metadata returned by paginated operations.
 *
 * **Details**
 *
 * It includes the base result metadata fields plus an optional `nextCursor`,
 * which indicates that more results may be available.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class PaginatedResultMeta extends PaginatedResultMeta_base {
}
/**
 * Schema for MCP conversation roles, allowing user and assistant.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const Role: Schema.Literals<["user", "assistant"]>;
/**
 * Type represented by the MCP role schema.
 *
 * **Details**
 *
 * Valid roles are `"user"` and `"assistant"`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Role = typeof Role.Type;
declare const Annotations_base: Schema.Opaque<Annotations, Schema.Struct<{
    /**
     * Describes who the intended customer of this object or data is.
     *
     * It can include multiple entries to indicate content useful for multiple
     * audiences (e.g., `["user", "assistant"]`).
     */
    readonly audience: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.Literals<["user", "assistant"]>>>>, Schema.optionalKey<Schema.$Array<Schema.Literals<["user", "assistant"]>>>, never, never>;
    /**
     * Describes how important this data is for operating the server.
     *
     * A value of 1 means "most important," and indicates that the data is
     * effectively required, while 0 means "least important," and indicates that
     * the data is entirely optional.
     */
    readonly priority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    /**
     * The last time the annotated object was modified, formatted as an ISO 8601
     * timestamp.
     */
    readonly lastModified: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * Describes who the intended customer of this object or data is.
     *
     * It can include multiple entries to indicate content useful for multiple
     * audiences (e.g., `["user", "assistant"]`).
     */
    readonly audience: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.Literals<["user", "assistant"]>>>>, Schema.optionalKey<Schema.$Array<Schema.Literals<["user", "assistant"]>>>, never, never>;
    /**
     * Describes how important this data is for operating the server.
     *
     * A value of 1 means "most important," and indicates that the data is
     * effectively required, while 0 means "least important," and indicates that
     * the data is entirely optional.
     */
    readonly priority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    /**
     * The last time the annotated object was modified, formatted as an ISO 8601
     * timestamp.
     */
    readonly lastModified: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for optional client-facing annotations on MCP objects.
 *
 * **When to use**
 *
 * Use to describe intended audience and priority metadata for objects shown or
 * processed by a client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Annotations extends Annotations_base {
}
declare const Icon_base: Schema.Class<Icon, Schema.Struct<{
    /**
     * URI containing the icon image.
     */
    readonly src: Schema.String;
    /**
     * MIME type of the icon, when known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Sizes supported by the icon, such as `"48x48"` or `"any"`.
     */
    readonly sizes: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    /**
     * Color theme for which the icon was designed.
     */
    readonly theme: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Literals<readonly ["light", "dark"]>>>, Schema.optionalKey<Schema.Literals<readonly ["light", "dark"]>>, never, never>;
}>, {}>;
/**
 * Schema for an icon that an MCP client can display.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Icon extends Icon_base {
}
declare const Implementation_base: Schema.Opaque<Implementation, Schema.Struct<{
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly version: Schema.String;
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly websiteUrl: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly version: Schema.String;
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly websiteUrl: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, keyof Schema.Top>;
/**
 * Describes the name and version of an MCP implementation.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Implementation extends Implementation_base {
}
declare const ClientCapabilities_base: Schema.Class<ClientCapabilities, Schema.Struct<{
    /**
     * Experimental, non-standard capabilities that the client supports.
     */
    readonly experimental: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Struct<{}>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Struct<{}>>>, never, never>;
    /**
     * Optional extensions capabilities advertised by the client.
     * Keys are extension identifiers following <vendor-prefix>/<extension-name> (e.g. "io.modelcontextprotocol/ui").
     */
    readonly extensions: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.TemplateLiteral<readonly [Schema.String, "/", Schema.String]>, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.TemplateLiteral<readonly [Schema.String, "/", Schema.String]>, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * Present if the client supports listing roots.
     */
    readonly roots: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Whether the client supports notifications for changes to the roots list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Whether the client supports notifications for changes to the roots list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>, never, never>;
    /**
     * Present if the client supports sampling from an LLM.
     */
    readonly sampling: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Present if the client supports context inclusion during sampling.
         */
        readonly context: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
        /**
         * Present if the client supports tool use during sampling.
         */
        readonly tools: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Present if the client supports context inclusion during sampling.
         */
        readonly context: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
        /**
         * Present if the client supports tool use during sampling.
         */
        readonly tools: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    }>>, never, never>;
    /**
     * Present if the client supports elicitation from the server.
     */
    readonly elicitation: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Present if the client supports form-mode elicitation.
         */
        readonly form: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
        /**
         * Present if the client supports URL-mode elicitation.
         */
        readonly url: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Present if the client supports form-mode elicitation.
         */
        readonly form: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
        /**
         * Present if the client supports URL-mode elicitation.
         */
        readonly url: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    }>>, never, never>;
}>, {}>;
/**
 * Describes capabilities advertised by an MCP client.
 *
 * **When to use**
 *
 * Use to describe which optional MCP features a client supports during
 * initialization.
 *
 * **Details**
 *
 * Known capabilities are represented by this schema, but the capability set is
 * open and clients may define additional capabilities.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ClientCapabilities extends ClientCapabilities_base {
}
declare const ServerCapabilities_base: Schema.Opaque<ServerCapabilities, Schema.Struct<{
    /**
     * Experimental, non-standard capabilities that the server supports.
     */
    readonly experimental: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Struct<{}>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Struct<{}>>>, never, never>;
    /**
     * Optional extensions capabilities advertised by the server.
     * Keys are extension identifiers following <vendor-prefix>/<extension-name> (e.g. "io.modelcontextprotocol/ui").
     */
    readonly extensions: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.TemplateLiteral<readonly [Schema.String, "/", Schema.String]>, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.TemplateLiteral<readonly [Schema.String, "/", Schema.String]>, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * Present if the server supports sending log messages to the client.
     */
    readonly logging: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    /**
     * Present if the server supports argument autocompletion suggestions.
     */
    readonly completions: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    /**
     * Present if the server offers any prompt templates.
     */
    readonly prompts: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the prompt list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the prompt list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>, never, never>;
    /**
     * Present if the server offers any resources to read.
     */
    readonly resources: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Whether this server supports subscribing to resource updates.
         */
        readonly subscribe: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        /**
         * Whether this server supports notifications for changes to the resource list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Whether this server supports subscribing to resource updates.
         */
        readonly subscribe: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        /**
         * Whether this server supports notifications for changes to the resource list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>, never, never>;
    /**
     * Present if the server offers any tools to call.
     */
    readonly tools: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the tool list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the tool list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * Experimental, non-standard capabilities that the server supports.
     */
    readonly experimental: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Struct<{}>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Struct<{}>>>, never, never>;
    /**
     * Optional extensions capabilities advertised by the server.
     * Keys are extension identifiers following <vendor-prefix>/<extension-name> (e.g. "io.modelcontextprotocol/ui").
     */
    readonly extensions: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.TemplateLiteral<readonly [Schema.String, "/", Schema.String]>, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.TemplateLiteral<readonly [Schema.String, "/", Schema.String]>, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * Present if the server supports sending log messages to the client.
     */
    readonly logging: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    /**
     * Present if the server supports argument autocompletion suggestions.
     */
    readonly completions: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{}>>>, Schema.optionalKey<Schema.Struct<{}>>, never, never>;
    /**
     * Present if the server offers any prompt templates.
     */
    readonly prompts: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the prompt list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the prompt list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>, never, never>;
    /**
     * Present if the server offers any resources to read.
     */
    readonly resources: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Whether this server supports subscribing to resource updates.
         */
        readonly subscribe: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        /**
         * Whether this server supports notifications for changes to the resource list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Whether this server supports subscribing to resource updates.
         */
        readonly subscribe: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        /**
         * Whether this server supports notifications for changes to the resource list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>, never, never>;
    /**
     * Present if the server offers any tools to call.
     */
    readonly tools: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the tool list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Whether this server supports notifications for changes to the tool list.
         */
        readonly listChanged: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Describes capabilities advertised by an MCP server.
 *
 * **When to use**
 *
 * Use to describe which optional MCP features a server supports during
 * initialization.
 *
 * **Details**
 *
 * Known capabilities are represented by this schema, but the capability set is
 * open and servers may define additional capabilities.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ServerCapabilities extends ServerCapabilities_base {
}
declare const McpErrorBase_base: Schema.Class<McpErrorBase, Schema.Struct<{
    /**
     * The error type that occurred.
     */
    readonly code: Schema.Int;
    /**
     * A short description of the error. The message SHOULD be limited to a
     * concise single sentence.
     */
    readonly message: Schema.String;
    /**
     * Additional information about the error. The value of this member is
     * defined by the sender (e.g. detailed error information, nested errors etc.).
     */
    readonly data: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Any>>, Schema.optionalKey<Schema.Any>, never, never>;
}>, {}>;
/**
 * Schema for MCP and JSON-RPC error objects.
 *
 * **Details**
 *
 * It contains the numeric error `code`, a concise `message`, and optional
 * sender-defined `data`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class McpErrorBase extends McpErrorBase_base {
}
/**
 * Represents the JSON-RPC error code for requests that are not valid request objects.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for a syntactically parsed
 * request object that fails request-shape validation.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const INVALID_REQUEST_ERROR_CODE: -32600;
/**
 * Represents the JSON-RPC error code for requests whose method does not exist or is not
 * available.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for a request whose
 * `method` is unknown or unavailable.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const METHOD_NOT_FOUND_ERROR_CODE: -32601;
/**
 * Represents the JSON-RPC error code for invalid method parameters.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for decoded request
 * parameters that fail method-specific validation.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const INVALID_PARAMS_ERROR_CODE: -32602;
/**
 * Represents the JSON-RPC error code for internal server errors.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for an unexpected
 * server-side failure.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const INTERNAL_ERROR_CODE: -32603;
/**
 * Represents the JSON-RPC error code for invalid JSON that could not be parsed.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response before a request object is
 * available because the JSON payload could not be parsed.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const PARSE_ERROR_CODE: -32700;
/**
 * Represents the MCP error code for HTTP headers that do not match the
 * corresponding request values.
 *
 * **When to use**
 *
 * Use when building an MCP error response for missing, malformed, or
 * mismatched request-routing headers.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export declare const HEADER_MISMATCH_ERROR_CODE: -32020;
declare const ParseError_base: Schema.Class<ParseError, Schema.Struct<{
    /**
     * A short description of the error. The message SHOULD be limited to a
     * concise single sentence.
     */
    readonly message: Schema.String;
    /**
     * Additional information about the error. The value of this member is
     * defined by the sender (e.g. detailed error information, nested errors etc.).
     */
    readonly data: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Any>>, Schema.optionalKey<Schema.Any>, never, never>;
    readonly _tag: Schema.tag<"ParseError">;
    readonly code: Schema.tag<-32700>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Represents an MCP/JSON-RPC error for invalid JSON that could not be parsed.
 *
 * **When to use**
 *
 * Use to report a JSON parse failure before a valid JSON-RPC request object is
 * available.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC parse error code `-32700`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class ParseError extends ParseError_base {
}
declare const InvalidRequest_base: Schema.Class<InvalidRequest, Schema.Struct<{
    /**
     * A short description of the error. The message SHOULD be limited to a
     * concise single sentence.
     */
    readonly message: Schema.String;
    /**
     * Additional information about the error. The value of this member is
     * defined by the sender (e.g. detailed error information, nested errors etc.).
     */
    readonly data: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Any>>, Schema.optionalKey<Schema.Any>, never, never>;
    readonly _tag: Schema.tag<"InvalidRequest">;
    readonly code: Schema.tag<-32600>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Represents an MCP/JSON-RPC error for a request object that is not valid.
 *
 * **When to use**
 *
 * Use to report a syntactically parsed JSON-RPC request that is not a valid
 * request object.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC invalid request code `-32600`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidRequest extends InvalidRequest_base {
}
declare const MethodNotFound_base: Schema.Class<MethodNotFound, Schema.Struct<{
    /**
     * A short description of the error. The message SHOULD be limited to a
     * concise single sentence.
     */
    readonly message: Schema.String;
    /**
     * Additional information about the error. The value of this member is
     * defined by the sender (e.g. detailed error information, nested errors etc.).
     */
    readonly data: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Any>>, Schema.optionalKey<Schema.Any>, never, never>;
    readonly _tag: Schema.tag<"MethodNotFound">;
    readonly code: Schema.tag<-32601>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Represents an MCP/JSON-RPC error for an unavailable method.
 *
 * **When to use**
 *
 * Use to report a JSON-RPC method that does not exist or is not available.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC method-not-found code `-32601`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class MethodNotFound extends MethodNotFound_base {
}
declare const InvalidParams_base: Schema.Class<InvalidParams, Schema.Struct<{
    /**
     * A short description of the error. The message SHOULD be limited to a
     * concise single sentence.
     */
    readonly message: Schema.String;
    /**
     * Additional information about the error. The value of this member is
     * defined by the sender (e.g. detailed error information, nested errors etc.).
     */
    readonly data: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Any>>, Schema.optionalKey<Schema.Any>, never, never>;
    readonly _tag: Schema.tag<"InvalidParams">;
    readonly code: Schema.tag<-32602>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Represents an MCP/JSON-RPC error for invalid method parameters.
 *
 * **When to use**
 *
 * Use to report a request whose method parameters do not match the method
 * schema.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC invalid params code `-32602`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InvalidParams extends InvalidParams_base {
}
declare const InternalError_base: Schema.Class<InternalError, Schema.Struct<{
    /**
     * A short description of the error. The message SHOULD be limited to a
     * concise single sentence.
     */
    readonly message: Schema.String;
    /**
     * Additional information about the error. The value of this member is
     * defined by the sender (e.g. detailed error information, nested errors etc.).
     */
    readonly data: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Any>>, Schema.optionalKey<Schema.Any>, never, never>;
    readonly _tag: Schema.tag<"InternalError">;
    readonly code: Schema.tag<-32603>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Represents an MCP/JSON-RPC error for unexpected internal server failures.
 *
 * **When to use**
 *
 * Use to report an unexpected server-side failure while handling a valid
 * request.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC internal error code `-32603` and includes
 * `InternalError.notImplemented` for unimplemented handlers.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class InternalError extends InternalError_base {
    static readonly notImplemented: InternalError;
}
/**
 * Schema for MCP protocol errors returned in JSON-RPC failure responses,
 * including standard protocol errors and custom `McpErrorBase` values.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare const McpError: Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>;
/**
 * Type represented by the MCP protocol error schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type McpError = typeof McpError.Type;
declare const Ping_base: Rpc.Rpc<"ping", Schema.UndefinedOr<typeof RequestMeta>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Represents an MCP ping request used to check whether the peer is still alive.
 *
 * **When to use**
 *
 * Use to implement client or server liveness checks.
 *
 * **Details**
 *
 * The receiver should respond promptly; otherwise the sender may disconnect.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class Ping extends Ping_base {
}
declare const InitializeResult_base: Schema.Opaque<InitializeResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The version of the Model Context Protocol that the server wants to use.
     * This may not match the version that the client requested. If the client
     * cannot support this version, it MUST disconnect.
     */
    readonly protocolVersion: Schema.String;
    readonly capabilities: typeof ServerCapabilities;
    readonly serverInfo: typeof Implementation;
    /**
     * Instructions describing how to use the server and its features.
     *
     * This can be used by clients to improve the LLM's understanding of available
     * tools, resources, etc. It can be thought of like a "hint" to the model.
     * For example, this information MAY be added to the system prompt.
     */
    readonly instructions: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The version of the Model Context Protocol that the server wants to use.
     * This may not match the version that the client requested. If the client
     * cannot support this version, it MUST disconnect.
     */
    readonly protocolVersion: Schema.String;
    readonly capabilities: typeof ServerCapabilities;
    readonly serverInfo: typeof Implementation;
    /**
     * Instructions describing how to use the server and its features.
     *
     * This can be used by clients to improve the LLM's understanding of available
     * tools, resources, etc. It can be thought of like a "hint" to the model.
     * For example, this information MAY be added to the system prompt.
     */
    readonly instructions: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for the server's response to an initialize request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class InitializeResult extends InitializeResult_base {
}
declare const Initialize_base: Rpc.Rpc<"initialize", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The latest version of the Model Context Protocol that the client
     * supports. The client MAY decide to support older versions as well.
     */
    protocolVersion: Schema.String;
    /**
     * Capabilities a client may support. Known capabilities are defined here,
     * in this schema, but this is not a closed set: any client can define its
     * own, additional capabilities.
     */
    capabilities: typeof ClientCapabilities;
    /**
     * Describes the name and version of an MCP implementation.
     */
    clientInfo: typeof Implementation;
}>, typeof InitializeResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to the server when it first connects, asking it to begin
 * initialization.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class Initialize extends Initialize_base {
}
declare const InitializedNotification_base: Rpc.Rpc<"notifications/initialized", Schema.UndefinedOr<typeof NotificationMeta>, Schema.Void, Schema.Never, never, never>;
/**
 * Sent from the client to the server after initialization has finished.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class InitializedNotification extends InitializedNotification_base {
}
declare const CancelledNotification_base: Rpc.Rpc<"notifications/cancelled", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The ID of the request to cancel.
     *
     * This MUST correspond to the ID of a request previously issued in the
     * same direction.
     */
    requestId: Schema.Union<[Schema.String, Schema.Finite]>;
    /**
     * An optional string describing the reason for the cancellation. This MAY
     * be logged or presented to the user.
     */
    reason: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, Schema.Void, Schema.Never, never, never>;
/**
 * Sent from either peer to cancel a previously issued request in the same
 * direction.
 *
 * **Details**
 *
 * The payload identifies the request to cancel and may include a
 * human-readable reason.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class CancelledNotification extends CancelledNotification_base {
}
declare const ProgressNotification_base: Rpc.Rpc<"notifications/progress", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The progress token which was given in the initial request, used to
     * associate this notification with the request that is proceeding.
     */
    progressToken: Schema.Union<[Schema.String, Schema.Finite]>;
    /**
     * The progress thus far. This should increase every time progress is made,
     * even if the total is unknown.
     */
    progress: Schema.Finite;
    /**
     * Total number of items to process (or total progress required), if known.
     */
    total: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    /**
     * An optional message describing the current progress.
     */
    message: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, Schema.Void, Schema.Never, never, never>;
/**
 * Sent from either peer to report progress for a long-running request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ProgressNotification extends ProgressNotification_base {
}
declare const Resource_base: Schema.Class<Resource, Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * A human-readable name for this resource.
     *
     * This can be used by clients to populate UI elements.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A description of what this resource represents.
     *
     * This can be used by clients to improve the LLM's understanding of available
     * resources. It can be thought of like a "hint" to the model.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    /**
     * Icons that clients can display for this resource.
     */
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    /**
     * The size of the raw resource content, in bytes (i.e., before base64
     * encoding or any tokenization), if known.
     *
     * This can be used by Hosts to display file sizes and estimate context
     * window usage.
     */
    readonly size: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    /**
     * Optional additional metadata for the client.
     *
     * This parameter name is reserved by MCP to allow clients and servers to
     * attach additional metadata to resources.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}>;
/**
 * Schema for a known resource that the server is capable of reading.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Resource extends Resource_base {
}
declare const ResourceTemplate_base: Schema.Class<ResourceTemplate, Schema.Struct<{
    /**
     * A URI template (according to RFC 6570) that can be used to construct
     * resource URIs.
     */
    readonly uriTemplate: Schema.String;
    /**
     * A human-readable name for the type of resource this template refers to.
     *
     * This can be used by clients to populate UI elements.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A description of what this template is for.
     *
     * This can be used by clients to improve the LLM's understanding of available
     * resources. It can be thought of like a "hint" to the model.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * The MIME type for all resources that match this template. This should only
     * be included if all resources matching this template have the same type.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    /**
     * Icons that clients can display for this resource template.
     */
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    /**
     * Optional additional metadata for the client.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}>;
/**
 * Schema for a template description of resources available on the server.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ResourceTemplate extends ResourceTemplate_base {
}
declare const ResourceContents_base: Schema.Opaque<ResourceContents, Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional additional metadata for the client.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional additional metadata for the client.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for the contents of a specific resource or sub-resource.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ResourceContents extends ResourceContents_base {
}
declare const TextResourceContents_base: Schema.Opaque<TextResourceContents, Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional additional metadata for the client.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The text of the item. This must only be set if the item can actually be
     * represented as text (not binary data).
     */
    readonly text: Schema.String;
}>, {}> & Omit<Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional additional metadata for the client.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The text of the item. This must only be set if the item can actually be
     * represented as text (not binary data).
     */
    readonly text: Schema.String;
}>, keyof Schema.Top>;
/**
 * Schema for text resource contents represented as a string.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class TextResourceContents extends TextResourceContents_base {
}
declare const BlobResourceContents_base: Schema.Opaque<BlobResourceContents, Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional additional metadata for the client.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The binary data of the item decoded from a base64-encoded string.
     */
    readonly blob: Schema.Uint8ArrayFromBase64;
}>, {}> & Omit<Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional additional metadata for the client.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The binary data of the item decoded from a base64-encoded string.
     */
    readonly blob: Schema.Uint8ArrayFromBase64;
}>, keyof Schema.Top>;
/**
 * Schema for binary resource contents represented as a `Uint8Array`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class BlobResourceContents extends BlobResourceContents_base {
}
declare const ListResourcesResult_base: Schema.Class<ListResourcesResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * An opaque token representing the pagination position after the last returned result.
     * If present, there may be more results available.
     */
    readonly nextCursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly resources: Schema.$Array<typeof Resource>;
}>, {}>;
/**
 * Schema for the server's response to a resources/list request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ListResourcesResult extends ListResourcesResult_base {
}
declare const ListResources_base: Rpc.Rpc<"resources/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListResourcesResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to request a list of resources the server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ListResources extends ListResources_base {
}
declare const ListResourceTemplatesResult_base: Schema.Class<ListResourceTemplatesResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * An opaque token representing the pagination position after the last returned result.
     * If present, there may be more results available.
     */
    readonly nextCursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly resourceTemplates: Schema.$Array<typeof ResourceTemplate>;
}>, {}>;
/**
 * Schema for the server's response to a resources/templates/list request from
 * the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ListResourceTemplatesResult extends ListResourceTemplatesResult_base {
}
declare const ListResourceTemplates_base: Rpc.Rpc<"resources/templates/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListResourceTemplatesResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to request a list of resource templates the server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ListResourceTemplates extends ListResourceTemplates_base {
}
declare const ReadResourceResult_base: Schema.Opaque<ReadResourceResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly contents: Schema.$Array<Schema.Union<readonly [typeof TextResourceContents, typeof BlobResourceContents]>>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly contents: Schema.$Array<Schema.Union<readonly [typeof TextResourceContents, typeof BlobResourceContents]>>;
}>, keyof Schema.Top>;
/**
 * Schema for the server's response to a resources/read request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ReadResourceResult extends ReadResourceResult_base {
}
declare const ReadResource_base: Rpc.Rpc<"resources/read", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to read. The URI can use any protocol; it is up
     * to the server how to interpret it.
     */
    uri: Schema.String;
}>, typeof ReadResourceResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to the server, to read a specific resource URI.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ReadResource extends ReadResource_base {
}
declare const ResourceListChangedNotification_base: Rpc.Rpc<"notifications/resources/list_changed", Schema.UndefinedOr<typeof NotificationMeta>, Schema.Void, Schema.Never, never, never>;
/**
 * Represents a notification that the server's resource list changed.
 *
 * **When to use**
 *
 * Use to notify clients that `resources/list` should be requested again.
 *
 * **Details**
 *
 * Servers may send this notification without a previous client subscription.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ResourceListChangedNotification extends ResourceListChangedNotification_base {
}
declare const Subscribe_base: Rpc.Rpc<"resources/subscribe", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to request resources/updated notifications from the
 * server whenever a particular resource changes.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class Subscribe extends Subscribe_base {
}
declare const Unsubscribe_base: Rpc.Rpc<"resources/unsubscribe", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to request cancellation of resources/updated
 * notifications from the server. This should follow a previous
 * resources/subscribe request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class Unsubscribe extends Unsubscribe_base {
}
declare const ResourceUpdatedNotification_base: Rpc.Rpc<"notifications/resources/updated", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The URI of the resource that has been updated. This might be a sub-resource of the one that the client actually subscribed to.
     */
    uri: Schema.String;
}>, Schema.Void, Schema.Never, never, never>;
/**
 * Sent from the server when a subscribed resource URI has changed.
 *
 * **Details**
 *
 * The URI may identify a sub-resource of the resource that the client
 * originally subscribed to.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ResourceUpdatedNotification extends ResourceUpdatedNotification_base {
}
declare const PromptArgument_base: Schema.Opaque<PromptArgument, Schema.Struct<{
    /**
     * The name of the argument.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A human-readable description of the argument.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Whether this argument must be provided.
     */
    readonly required: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * The name of the argument.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A human-readable description of the argument.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Whether this argument must be provided.
     */
    readonly required: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, keyof Schema.Top>;
/**
 * Describes an argument that a prompt can accept.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class PromptArgument extends PromptArgument_base {
}
declare const Prompt_base: Schema.Class<Prompt, Schema.Struct<{
    /**
     * The name of the prompt or prompt template.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * An optional description of what this prompt provides
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A list of arguments to use for templating the prompt.
     */
    readonly arguments: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof PromptArgument>>>, Schema.optionalKey<Schema.$Array<typeof PromptArgument>>, never, never>;
    /**
     * Icons that clients can display for this prompt.
     */
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}>;
/**
 * Represents a prompt or prompt template that the server offers.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Prompt extends Prompt_base {
}
declare const TextContent_base: Schema.Opaque<TextContent, Schema.Struct<{
    readonly type: Schema.tag<"text">;
    /**
     * The text content of the message.
     */
    readonly text: Schema.String;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly type: Schema.tag<"text">;
    /**
     * The text content of the message.
     */
    readonly text: Schema.String;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Represents text content provided to or from an LLM.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class TextContent extends TextContent_base {
}
declare const ImageContent_base: Schema.Opaque<ImageContent, Schema.Struct<{
    readonly type: Schema.tag<"image">;
    /**
     * The image data.
     */
    readonly data: Schema.Uint8ArrayFromBase64;
    /**
     * The MIME type of the image. Different providers may support different
     * image types.
     */
    readonly mimeType: Schema.String;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly type: Schema.tag<"image">;
    /**
     * The image data.
     */
    readonly data: Schema.Uint8ArrayFromBase64;
    /**
     * The MIME type of the image. Different providers may support different
     * image types.
     */
    readonly mimeType: Schema.String;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Represents image content provided to or from an LLM.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ImageContent extends ImageContent_base {
}
declare const AudioContent_base: Schema.Opaque<AudioContent, Schema.Struct<{
    readonly type: Schema.tag<"audio">;
    /**
     * The audio data.
     */
    readonly data: Schema.Uint8ArrayFromBase64;
    /**
     * The MIME type of the audio. Different providers may support different
     * audio types.
     */
    readonly mimeType: Schema.String;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly type: Schema.tag<"audio">;
    /**
     * The audio data.
     */
    readonly data: Schema.Uint8ArrayFromBase64;
    /**
     * The MIME type of the audio. Different providers may support different
     * audio types.
     */
    readonly mimeType: Schema.String;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Represents audio content provided to or from an LLM.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class AudioContent extends AudioContent_base {
}
declare const EmbeddedResource_base: Schema.Opaque<EmbeddedResource, Schema.Struct<{
    readonly type: Schema.tag<"resource">;
    readonly resource: Schema.Union<readonly [typeof TextResourceContents, typeof BlobResourceContents]>;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly type: Schema.tag<"resource">;
    readonly resource: Schema.Union<readonly [typeof TextResourceContents, typeof BlobResourceContents]>;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Represents resource contents embedded into a prompt or tool call result.
 *
 * **Details**
 *
 * It is up to the client how best to render embedded resources for the benefit
 * of the LLM and/or the user.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class EmbeddedResource extends EmbeddedResource_base {
}
declare const ResourceLink_base: Schema.Opaque<ResourceLink, Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * A human-readable name for this resource.
     *
     * This can be used by clients to populate UI elements.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A description of what this resource represents.
     *
     * This can be used by clients to improve the LLM's understanding of available
     * resources. It can be thought of like a "hint" to the model.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    /**
     * Icons that clients can display for this resource.
     */
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    /**
     * The size of the raw resource content, in bytes (i.e., before base64
     * encoding or any tokenization), if known.
     *
     * This can be used by Hosts to display file sizes and estimate context
     * window usage.
     */
    readonly size: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    /**
     * Optional additional metadata for the client.
     *
     * This parameter name is reserved by MCP to allow clients and servers to
     * attach additional metadata to resources.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly type: Schema.tag<"resource_link">;
}>, {}> & Omit<Schema.Struct<{
    /**
     * The URI of this resource.
     */
    readonly uri: Schema.String;
    /**
     * A human-readable name for this resource.
     *
     * This can be used by clients to populate UI elements.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A description of what this resource represents.
     *
     * This can be used by clients to improve the LLM's understanding of available
     * resources. It can be thought of like a "hint" to the model.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * The MIME type of this resource, if known.
     */
    readonly mimeType: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional annotations for the client.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof Annotations>>, Schema.optionalKey<typeof Annotations>, never, never>;
    /**
     * Icons that clients can display for this resource.
     */
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    /**
     * The size of the raw resource content, in bytes (i.e., before base64
     * encoding or any tokenization), if known.
     *
     * This can be used by Hosts to display file sizes and estimate context
     * window usage.
     */
    readonly size: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    /**
     * Optional additional metadata for the client.
     *
     * This parameter name is reserved by MCP to allow clients and servers to
     * attach additional metadata to resources.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly type: Schema.tag<"resource_link">;
}>, keyof Schema.Top>;
/**
 * Represents a readable resource included in a prompt or tool call result.
 *
 * **Gotchas**
 *
 * Resource links returned by tools are not guaranteed to appear in the results
 * of `resources/list` requests.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ResourceLink extends ResourceLink_base {
}
/**
 * Schema for MCP content blocks that can appear in prompt messages or tool
 * results.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const ContentBlock: Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof EmbeddedResource, typeof ResourceLink]>;
/**
 * Type represented by the MCP content block schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ContentBlock = typeof ContentBlock.Type;
declare const PromptMessage_base: Schema.Opaque<PromptMessage, Schema.Struct<{
    readonly role: Schema.Literals<["user", "assistant"]>;
    readonly content: Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof EmbeddedResource, typeof ResourceLink]>;
}>, {}> & Omit<Schema.Struct<{
    readonly role: Schema.Literals<["user", "assistant"]>;
    readonly content: Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof EmbeddedResource, typeof ResourceLink]>;
}>, keyof Schema.Top>;
/**
 * Describes a message returned as part of a prompt.
 *
 * **Details**
 *
 * This is similar to `SamplingMessage`, but also supports the embedding of
 * resources from the MCP server.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class PromptMessage extends PromptMessage_base {
}
declare const ListPromptsResult_base: Schema.Class<ListPromptsResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * An opaque token representing the pagination position after the last returned result.
     * If present, there may be more results available.
     */
    readonly nextCursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly prompts: Schema.$Array<typeof Prompt>;
}>, {}>;
/**
 * Represents the server response to a prompts/list request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ListPromptsResult extends ListPromptsResult_base {
}
declare const ListPrompts_base: Rpc.Rpc<"prompts/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListPromptsResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to request a list of prompts and prompt templates the
 * server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ListPrompts extends ListPrompts_base {
}
declare const GetPromptResult_base: Schema.Class<GetPromptResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly messages: Schema.$Array<typeof PromptMessage>;
    /**
     * An optional description for the prompt.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}>;
/**
 * Represents the server response to a prompts/get request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class GetPromptResult extends GetPromptResult_base {
}
declare const GetPrompt_base: Rpc.Rpc<"prompts/get", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The name of the prompt or prompt template.
     */
    name: Schema.String;
    title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Arguments to use for templating the prompt.
     */
    arguments: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
}>, typeof GetPromptResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to get a prompt provided by the server.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class GetPrompt extends GetPrompt_base {
}
declare const PromptListChangedNotification_base: Rpc.Rpc<"notifications/prompts/list_changed", Schema.UndefinedOr<typeof NotificationMeta>, Schema.Void, Schema.Never, never, never>;
/**
 * Represents a notification that the server's prompt list changed.
 *
 * **When to use**
 *
 * Use to notify clients that `prompts/list` should be requested again.
 *
 * **Details**
 *
 * Servers may send this notification without a previous client subscription.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class PromptListChangedNotification extends PromptListChangedNotification_base {
}
declare const ToolAnnotations_base: Schema.Opaque<ToolAnnotations, Schema.Struct<{
    /**
     * A human-readable title for the tool.
     */
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * If true, the tool does not modify its environment.
     *
     * Default: `false`
     */
    readonly readOnlyHint: optionalWithDefault<Schema.Boolean>;
    /**
     * If true, the tool may perform destructive updates to its environment.
     * If false, the tool performs only additive updates.
     *
     * (This property is meaningful only when `readOnlyHint == false`)
     *
     * Default: `true`
     */
    readonly destructiveHint: optionalWithDefault<Schema.Boolean>;
    /**
     * If true, calling the tool repeatedly with the same arguments
     * will have no additional effect on the its environment.
     *
     * (This property is meaningful only when `readOnlyHint == false`)
     *
     * Default: `false`
     */
    readonly idempotentHint: optionalWithDefault<Schema.Boolean>;
    /**
     * If true, this tool may interact with an "open world" of external
     * entities. If false, the tool's domain of interaction is closed.
     * For example, the world of a web search tool is open, whereas that
     * of a memory tool is not.
     *
     * Default: `true`
     */
    readonly openWorldHint: optionalWithDefault<Schema.Boolean>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * A human-readable title for the tool.
     */
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * If true, the tool does not modify its environment.
     *
     * Default: `false`
     */
    readonly readOnlyHint: optionalWithDefault<Schema.Boolean>;
    /**
     * If true, the tool may perform destructive updates to its environment.
     * If false, the tool performs only additive updates.
     *
     * (This property is meaningful only when `readOnlyHint == false`)
     *
     * Default: `true`
     */
    readonly destructiveHint: optionalWithDefault<Schema.Boolean>;
    /**
     * If true, calling the tool repeatedly with the same arguments
     * will have no additional effect on the its environment.
     *
     * (This property is meaningful only when `readOnlyHint == false`)
     *
     * Default: `false`
     */
    readonly idempotentHint: optionalWithDefault<Schema.Boolean>;
    /**
     * If true, this tool may interact with an "open world" of external
     * entities. If false, the tool's domain of interaction is closed.
     * For example, the world of a web search tool is open, whereas that
     * of a memory tool is not.
     *
     * Default: `true`
     */
    readonly openWorldHint: optionalWithDefault<Schema.Boolean>;
}>, keyof Schema.Top>;
/**
 * Schema for additional properties describing a tool to clients.
 *
 * **Details**
 *
 * NOTE: all properties in ToolAnnotations are **hints**. They are not
 * guaranteed to provide a faithful description of tool behavior (including
 * descriptive properties like `title`).
 *
 * **Gotchas**
 *
 * Clients should never make tool use decisions based on ToolAnnotations
 * received from untrusted servers.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ToolAnnotations extends ToolAnnotations_base {
}
/**
 * Object-root JSON Schema used by MCP tool inputs.
 *
 * **Details**
 *
 * Property definitions and additional root keywords are constrained to JSON
 * values. The open root supports generated keywords such as `$defs`.
 *
 * @stability unstable
 * @category tools
 * @since 4.0.0
 */
export type ToolJson = Schema.JsonObject & {
    readonly type: "object";
    readonly properties?: Readonly<Record<string, Schema.JsonObject>> | undefined;
    readonly required?: ReadonlyArray<string> | undefined;
};
/**
 * Schema for {@link ToolJson}.
 *
 * @stability unstable
 * @category tools
 * @since 4.0.0
 */
export declare const ToolJson: Schema.Codec<ToolJson>;
/**
 * JSON Schema used by MCP tool outputs.
 *
 * **Details**
 *
 * Unlike tool inputs, tool outputs may use any JSON Schema root type.
 *
 * @stability unstable
 * @category tools
 * @since 4.0.0
 */
export type ToolOutputJson = Schema.JsonObject;
/**
 * Schema for {@link ToolOutputJson}.
 *
 * @stability unstable
 * @category tools
 * @since 4.0.0
 */
export declare const ToolOutputJson: Schema.Codec<ToolOutputJson>;
declare const Tool_base: Schema.Class<Tool, Schema.Struct<{
    /**
     * The name of the tool.
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A human-readable description of the tool.
     *
     * This can be used by clients to improve the LLM's understanding of available tools. It can be thought of like a "hint" to the model.
     */
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A JSON Schema object defining the expected parameters for the tool.
     */
    readonly inputSchema: Schema.Codec<ToolJson, ToolJson, never, never>;
    /**
     * An optional JSON Schema object defining the structure of the tool output.
     */
    readonly outputSchema: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Codec<Schema.JsonObject, Schema.JsonObject, never, never>>>, Schema.optionalKey<Schema.Codec<Schema.JsonObject, Schema.JsonObject, never, never>>, never, never>;
    /**
     * Optional additional tool information.
     */
    readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof ToolAnnotations>>, Schema.optionalKey<typeof ToolAnnotations>, never, never>;
    /**
     * Icons that clients can display for this tool.
     */
    readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
    /**
     * Optional additional metadata for the client.
     *
     * This parameter name is reserved by MCP to allow clients and servers to
     * attach additional metadata to resources.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}>;
/**
 * Schema for the definition of a tool the client can call.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Tool extends Tool_base {
}
declare const ListToolsResult_base: Schema.Class<ListToolsResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * An opaque token representing the pagination position after the last returned result.
     * If present, there may be more results available.
     */
    readonly nextCursor: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly tools: Schema.$Array<typeof Tool>;
}>, {}>;
/**
 * Schema for the server's response to a tools/list request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ListToolsResult extends ListToolsResult_base {
}
declare const ListTools_base: Rpc.Rpc<"tools/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListToolsResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to request a list of tools the server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ListTools extends ListTools_base {
}
declare const CallToolResult_base: Schema.Class<CallToolResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly content: Schema.$Array<Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof EmbeddedResource, typeof ResourceLink]>>;
    /**
     * An optional JSON value containing the structured result of the tool call.
     */
    readonly structuredContent: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Codec<Schema.Json, Schema.Json, never, never>>>, Schema.optionalKey<Schema.Codec<Schema.Json, Schema.Json, never, never>>, never, never>;
    /**
     * Whether the tool call ended in an error.
     *
     * If not set, this is assumed to be false (the call was successful).
     */
    readonly isError: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, {}>;
/**
 * Schema for the server's response to a tool call.
 *
 * **Details**
 *
 * Any errors that originate from the tool SHOULD be reported inside the result
 * object, with `isError` set to true, _not_ as an MCP protocol-level error
 * response. Otherwise, the LLM would not be able to see that an error occurred
 * and self-correct. However, any errors in _finding_ the tool, an error
 * indicating that the server does not support tool calls, or any other
 * exceptional conditions, should be reported as an MCP error response.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class CallToolResult extends CallToolResult_base {
}
declare const CallTool_base: Rpc.Rpc<"tools/call", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    name: Schema.String;
    arguments: optionalWithDefault<Schema.$Record<Schema.String, Schema.Any>>;
}>, typeof CallToolResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Represents a client request to invoke a tool provided by the server.
 *
 * **When to use**
 *
 * Use when you need to represent a client request that already knows the tool
 * name and asks the server to execute it with argument values.
 *
 * @see {@link ListTools} for discovering available tools before calling one
 * @see {@link CallToolResult} for the successful tool-call result shape
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class CallTool extends CallTool_base {
}
declare const ToolListChangedNotification_base: Rpc.Rpc<"notifications/tools/list_changed", Schema.UndefinedOr<typeof NotificationMeta>, Schema.Void, Schema.Never, never, never>;
/**
 * Represents a notification that the server's tool list changed.
 *
 * **When to use**
 *
 * Use to notify clients that `tools/list` should be requested again.
 *
 * **Details**
 *
 * Servers may send this notification without a previous client subscription.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ToolListChangedNotification extends ToolListChangedNotification_base {
}
/**
 * Schema for log message severity levels, mapped to syslog message severities
 * as specified in RFC 5424 section 6.2.1:
 * https://datatracker.ietf.org/doc/html/rfc5424#section-6.2.1.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export declare const LoggingLevel: Schema.Literals<[
    "debug",
    "info",
    "notice",
    "warning",
    "error",
    "critical",
    "alert",
    "emergency"
]>;
/**
 * Type represented by the MCP logging level schema, mapped to syslog message
 * severities as specified in RFC 5424 section 6.2.1:
 * https://datatracker.ietf.org/doc/html/rfc5424#section-6.2.1.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export type LoggingLevel = typeof LoggingLevel.Type;
declare const SetLevel_base: Rpc.Rpc<"logging/setLevel", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The level of logging that the client wants to receive from the server.
     * The server should send all logs at this level and higher (i.e., more
     * severe) to the client as notifications/message.
     */
    level: Schema.Literals<["debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"]>;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to the server to enable or adjust logging.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export declare class SetLevel extends SetLevel_base {
}
declare const LoggingMessageNotification_base: Rpc.Rpc<"notifications/message", Schema.Struct<{
    /**
     * This parameter name is reserved by MCP to allow clients and servers to
     * attach additional metadata to their notifications.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The severity of this log message.
     */
    readonly level: Schema.Literals<["debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"]>;
    /**
     * An optional name of the logger issuing this message.
     */
    readonly logger: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * The data to be logged, such as a string message or an object. Any JSON
     * serializable type is allowed here.
     */
    readonly data: Schema.Any;
}>, Schema.Void, Schema.Never, never, never>;
/**
 * Sent from the server to the client carrying a log message.
 *
 * **Details**
 *
 * The notification includes the severity level, optional logger name, and
 * JSON-serializable log data.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export declare class LoggingMessageNotification extends LoggingMessageNotification_base {
}
declare const ToolUseContent_base: Schema.Class<ToolUseContent, Schema.Struct<{
    readonly type: Schema.tag<"tool_use">;
    /**
     * Identifier used to associate a later tool result with this request.
     */
    readonly id: Schema.String;
    /**
     * Name of the tool to invoke.
     */
    readonly name: Schema.String;
    /**
     * Arguments supplied to the tool.
     */
    readonly input: Schema.$Record<Schema.String, Schema.Unknown>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}>;
/**
 * Schema for a tool-use request produced during MCP sampling.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export declare class ToolUseContent extends ToolUseContent_base {
}
declare const ToolResultContent_base: Schema.Class<ToolResultContent, Schema.Struct<{
    readonly type: Schema.tag<"tool_result">;
    /**
     * Identifier of the tool-use request that produced this result.
     */
    readonly toolUseId: Schema.String;
    /**
     * Content returned by the tool.
     */
    readonly content: Schema.$Array<Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof EmbeddedResource, typeof ResourceLink]>>;
    /**
     * Optional structured result returned by the tool.
     */
    readonly structuredContent: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Codec<Schema.Json, Schema.Json, never, never>>>, Schema.optionalKey<Schema.Codec<Schema.Json, Schema.Json, never, never>>, never, never>;
    /**
     * Whether tool execution ended in an error.
     */
    readonly isError: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}>;
/**
 * Schema for the result of a tool use supplied in a sampling message.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export declare class ToolResultContent extends ToolResultContent_base {
}
/**
 * Schema for content blocks accepted in MCP sampling messages.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export declare const SamplingMessageContentBlock: Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof ToolUseContent, typeof ToolResultContent]>;
/**
 * Type represented by the MCP sampling message content block schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type SamplingMessageContentBlock = typeof SamplingMessageContentBlock.Type;
declare const SamplingMessage_base: Schema.Opaque<SamplingMessage, Schema.Struct<{
    readonly role: Schema.Literals<["user", "assistant"]>;
    readonly content: Schema.Union<readonly [Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof ToolUseContent, typeof ToolResultContent]>, Schema.$Array<Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof ToolUseContent, typeof ToolResultContent]>>]>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly role: Schema.Literals<["user", "assistant"]>;
    readonly content: Schema.Union<readonly [Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof ToolUseContent, typeof ToolResultContent]>, Schema.$Array<Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof ToolUseContent, typeof ToolResultContent]>>]>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, keyof Schema.Top>;
/**
 * Describes a message issued to or received from an LLM API.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class SamplingMessage extends SamplingMessage_base {
}
declare const ToolChoice_base: Schema.Class<ToolChoice, Schema.Struct<{
    /**
     * Tool-selection mode requested from the client.
     */
    readonly mode: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Literals<readonly ["auto", "required", "none"]>>>, Schema.optionalKey<Schema.Literals<readonly ["auto", "required", "none"]>>, never, never>;
}>, {}>;
/**
 * Schema for controlling tool selection during MCP sampling.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export declare class ToolChoice extends ToolChoice_base {
}
declare const ModelHint_base: Schema.Opaque<ModelHint, Schema.Struct<{
    /**
     * A hint for a model name.
     *
     * The client SHOULD treat this as a substring of a model name; for example:
     *  - `claude-3-5-sonnet` should match `claude-3-5-sonnet-20241022`
     *  - `sonnet` should match `claude-3-5-sonnet-20241022`, `claude-3-sonnet-20240229`, etc.
     *  - `claude` should match any Claude model
     *
     * The client MAY also map the string to a different provider's model name or
     * a different model family, as long as it fills a similar niche; for example:
     *  - `gemini-1.5-flash` could match `claude-3-haiku-20240307`
     */
    readonly name: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * A hint for a model name.
     *
     * The client SHOULD treat this as a substring of a model name; for example:
     *  - `claude-3-5-sonnet` should match `claude-3-5-sonnet-20241022`
     *  - `sonnet` should match `claude-3-5-sonnet-20241022`, `claude-3-sonnet-20240229`, etc.
     *  - `claude` should match any Claude model
     *
     * The client MAY also map the string to a different provider's model name or
     * a different model family, as long as it fills a similar niche; for example:
     *  - `gemini-1.5-flash` could match `claude-3-haiku-20240307`
     */
    readonly name: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for model selection hints.
 *
 * **Details**
 *
 * Keys not declared here are currently left unspecified by the spec and are up
 * to the client to interpret.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ModelHint extends ModelHint_base {
}
declare const ModelPreferences_base: Schema.Class<ModelPreferences, Schema.Struct<{
    /**
     * Optional hints to use for model selection.
     *
     * If multiple hints are specified, the client MUST evaluate them in order
     * (such that the first match is taken).
     *
     * The client SHOULD prioritize these hints over the numeric priorities, but
     * MAY still use the priorities to select from ambiguous matches.
     */
    readonly hints: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof ModelHint>>>, Schema.optionalKey<Schema.$Array<typeof ModelHint>>, never, never>;
    /**
     * How much to prioritize cost when selecting a model. A value of 0 means cost
     * is not important, while a value of 1 means cost is the most important
     * factor.
     */
    readonly costPriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    /**
     * How much to prioritize sampling speed (latency) when selecting a model. A
     * value of 0 means speed is not important, while a value of 1 means speed is
     * the most important factor.
     */
    readonly speedPriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    /**
     * How much to prioritize intelligence and capabilities when selecting a
     * model. A value of 0 means intelligence is not important, while a value of 1
     * means intelligence is the most important factor.
     */
    readonly intelligencePriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
}>, {}>;
/**
 * Schema for the server's model selection preferences requested of the client
 * during sampling.
 *
 * **Details**
 *
 * Because LLMs can vary along multiple dimensions, choosing the "best" model is
 * rarely straightforward. Different models excel in different areas, some are
 * faster but less capable, others are more capable but more expensive, and so
 * on. This interface allows servers to express their priorities across multiple
 * dimensions to help clients make an appropriate selection for their use case.
 *
 * **Gotchas**
 *
 * These preferences are always advisory. The client MAY ignore them. It is also
 * up to the client to decide how to interpret these preferences and how to
 * balance them against other considerations.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ModelPreferences extends ModelPreferences_base {
}
declare const CreateMessageResult_base: Schema.Class<CreateMessageResult, Schema.Struct<{
    readonly role: Schema.Literals<["user", "assistant"]>;
    readonly content: Schema.Union<readonly [Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof ToolUseContent, typeof ToolResultContent]>, Schema.$Array<Schema.Union<readonly [typeof TextContent, typeof ImageContent, typeof AudioContent, typeof ToolUseContent, typeof ToolResultContent]>>]>;
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The name of the model that generated the message.
     */
    readonly model: Schema.String;
    /**
     * The reason why sampling stopped, if known.
     */
    readonly stopReason: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}>;
/**
 * Represents a client response to an MCP sampling request.
 *
 * **When to use**
 *
 * Use to return the message produced by client-side model sampling.
 *
 * **Details**
 *
 * The client should let the user inspect the sampled message before returning
 * it to the server.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class CreateMessageResult extends CreateMessageResult_base {
}
declare const CreateMessage_base: Rpc.Rpc<"sampling/createMessage", Schema.Struct<{
    messages: Schema.$Array<typeof SamplingMessage>;
    /**
     * The server's preferences for which model to select. The client MAY ignore
     * these preferences.
     */
    modelPreferences: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Optional hints to use for model selection.
         *
         * If multiple hints are specified, the client MUST evaluate them in order
         * (such that the first match is taken).
         *
         * The client SHOULD prioritize these hints over the numeric priorities, but
         * MAY still use the priorities to select from ambiguous matches.
         */
        readonly hints: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof ModelHint>>>, Schema.optionalKey<Schema.$Array<typeof ModelHint>>, never, never>;
        /**
         * How much to prioritize cost when selecting a model. A value of 0 means cost
         * is not important, while a value of 1 means cost is the most important
         * factor.
         */
        readonly costPriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
        /**
         * How much to prioritize sampling speed (latency) when selecting a model. A
         * value of 0 means speed is not important, while a value of 1 means speed is
         * the most important factor.
         */
        readonly speedPriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
        /**
         * How much to prioritize intelligence and capabilities when selecting a
         * model. A value of 0 means intelligence is not important, while a value of 1
         * means intelligence is the most important factor.
         */
        readonly intelligencePriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Optional hints to use for model selection.
         *
         * If multiple hints are specified, the client MUST evaluate them in order
         * (such that the first match is taken).
         *
         * The client SHOULD prioritize these hints over the numeric priorities, but
         * MAY still use the priorities to select from ambiguous matches.
         */
        readonly hints: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof ModelHint>>>, Schema.optionalKey<Schema.$Array<typeof ModelHint>>, never, never>;
        /**
         * How much to prioritize cost when selecting a model. A value of 0 means cost
         * is not important, while a value of 1 means cost is the most important
         * factor.
         */
        readonly costPriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
        /**
         * How much to prioritize sampling speed (latency) when selecting a model. A
         * value of 0 means speed is not important, while a value of 1 means speed is
         * the most important factor.
         */
        readonly speedPriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
        /**
         * How much to prioritize intelligence and capabilities when selecting a
         * model. A value of 0 means intelligence is not important, while a value of 1
         * means intelligence is the most important factor.
         */
        readonly intelligencePriority: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    }>>, never, never>;
    /**
     * An optional system prompt the server wants to use for sampling. The
     * client MAY modify or omit this prompt.
     */
    systemPrompt: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * A request to include context from one or more MCP servers (including the
     * caller), to be attached to the prompt. The client MAY ignore this request.
     */
    includeContext: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Literals<readonly ["none", "thisServer", "allServers"]>>>, Schema.optionalKey<Schema.Literals<readonly ["none", "thisServer", "allServers"]>>, never, never>;
    temperature: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    /**
     * The maximum number of tokens to sample, as requested by the server. The
     * client MAY choose to sample fewer tokens than requested.
     */
    maxTokens: Schema.Int;
    stopSequences: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    /**
     * Optional metadata to pass through to the LLM provider. The format of
     * this metadata is provider-specific.
     */
    metadata: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    /**
     * Tools that the model may call while producing the response.
     */
    tools: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.Struct<{
        /**
         * The name of the tool.
         */
        readonly name: Schema.String;
        readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        /**
         * A human-readable description of the tool.
         *
         * This can be used by clients to improve the LLM's understanding of available tools. It can be thought of like a "hint" to the model.
         */
        readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        /**
         * A JSON Schema object defining the expected parameters for the tool.
         */
        readonly inputSchema: Schema.Codec<ToolJson, ToolJson, never, never>;
        /**
         * An optional JSON Schema object defining the structure of the tool output.
         */
        readonly outputSchema: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Codec<Schema.JsonObject, Schema.JsonObject, never, never>>>, Schema.optionalKey<Schema.Codec<Schema.JsonObject, Schema.JsonObject, never, never>>, never, never>;
        /**
         * Optional additional tool information.
         */
        readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof ToolAnnotations>>, Schema.optionalKey<typeof ToolAnnotations>, never, never>;
        /**
         * Icons that clients can display for this tool.
         */
        readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
        /**
         * Optional additional metadata for the client.
         *
         * This parameter name is reserved by MCP to allow clients and servers to
         * attach additional metadata to resources.
         */
        readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
        /**
         * The name of the tool.
         */
        readonly name: Schema.String;
        readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        /**
         * A human-readable description of the tool.
         *
         * This can be used by clients to improve the LLM's understanding of available tools. It can be thought of like a "hint" to the model.
         */
        readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        /**
         * A JSON Schema object defining the expected parameters for the tool.
         */
        readonly inputSchema: Schema.Codec<ToolJson, ToolJson, never, never>;
        /**
         * An optional JSON Schema object defining the structure of the tool output.
         */
        readonly outputSchema: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Codec<Schema.JsonObject, Schema.JsonObject, never, never>>>, Schema.optionalKey<Schema.Codec<Schema.JsonObject, Schema.JsonObject, never, never>>, never, never>;
        /**
         * Optional additional tool information.
         */
        readonly annotations: Schema.decodeTo<Schema.toType<Schema.optional<typeof ToolAnnotations>>, Schema.optionalKey<typeof ToolAnnotations>, never, never>;
        /**
         * Icons that clients can display for this tool.
         */
        readonly icons: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<typeof Icon>>>, Schema.optionalKey<Schema.$Array<typeof Icon>>, never, never>;
        /**
         * Optional additional metadata for the client.
         *
         * This parameter name is reserved by MCP to allow clients and servers to
         * attach additional metadata to resources.
         */
        readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    }>>>, never, never>;
    /**
     * Controls whether the model may or must call a tool.
     */
    toolChoice: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Struct<{
        /**
         * Tool-selection mode requested from the client.
         */
        readonly mode: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Literals<readonly ["auto", "required", "none"]>>>, Schema.optionalKey<Schema.Literals<readonly ["auto", "required", "none"]>>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        /**
         * Tool-selection mode requested from the client.
         */
        readonly mode: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Literals<readonly ["auto", "required", "none"]>>>, Schema.optionalKey<Schema.Literals<readonly ["auto", "required", "none"]>>, never, never>;
    }>>, never, never>;
}>, typeof CreateMessageResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Represents a server request for the client to sample an LLM.
 *
 * **When to use**
 *
 * Use when you need to request model sampling from an MCP client on behalf of a
 * server.
 *
 * **Details**
 *
 * The client chooses the model and should ask the user to approve the sampling
 * request before it begins.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class CreateMessage extends CreateMessage_base {
}
declare const ResourceReference_base: Schema.Opaque<ResourceReference, Schema.Struct<{
    readonly type: Schema.tag<"ref/resource">;
    /**
     * The URI or URI template of the resource.
     */
    readonly uri: Schema.String;
}>, {}> & Omit<Schema.Struct<{
    readonly type: Schema.tag<"ref/resource">;
    /**
     * The URI or URI template of the resource.
     */
    readonly uri: Schema.String;
}>, keyof Schema.Top>;
/**
 * Schema for a reference to a resource or resource template definition.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ResourceReference extends ResourceReference_base {
}
declare const PromptReference_base: Schema.Opaque<PromptReference, Schema.Struct<{
    readonly type: Schema.tag<"ref/prompt">;
    /**
     * The name of the prompt or prompt template
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}> & Omit<Schema.Struct<{
    readonly type: Schema.tag<"ref/prompt">;
    /**
     * The name of the prompt or prompt template
     */
    readonly name: Schema.String;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, keyof Schema.Top>;
/**
 * Schema for a prompt reference used in autocomplete requests.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class PromptReference extends PromptReference_base {
}
declare const CompleteResult_base: Schema.Opaque<CompleteResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly completion: Schema.Struct<{
        /**
         * An array of completion values. Must not exceed 100 items.
         */
        readonly values: Schema.$Array<Schema.String>;
        /**
         * The total number of completion options available. This can exceed the
         * number of values actually sent in the response.
         */
        readonly total: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
        /**
         * Indicates whether there are additional completion options beyond those
         * provided in the current response, even if the exact total is unknown.
         */
        readonly hasMore: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>;
}>, {}> & Omit<Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    readonly completion: Schema.Struct<{
        /**
         * An array of completion values. Must not exceed 100 items.
         */
        readonly values: Schema.$Array<Schema.String>;
        /**
         * The total number of completion options available. This can exceed the
         * number of values actually sent in the response.
         */
        readonly total: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
        /**
         * Indicates whether there are additional completion options beyond those
         * provided in the current response, even if the exact total is unknown.
         */
        readonly hasMore: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>;
}>, keyof Schema.Top>;
/**
 * Schema for the server's response to a completion/complete request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class CompleteResult extends CompleteResult_base {
    /**
     * Empty completion result used when a completion request has no values.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static readonly empty: CompleteResult;
}
declare const Complete_base: Rpc.Rpc<"completion/complete", Schema.Struct<{
    readonly ref: Schema.Union<readonly [typeof PromptReference, typeof ResourceReference]>;
    /**
     * The argument's information
     */
    readonly argument: Schema.Struct<{
        /**
         * The name of the argument
         */
        readonly name: Schema.String;
        /**
         * The value of the argument to use for completion matching.
         */
        readonly value: Schema.String;
    }>;
    /**
     * Additional, optional context for completions
     */
    readonly context: optionalWithDefault<Schema.Struct<{
        /**
         * Previously-resolved variables in a URI template or prompt.
         */
        readonly arguments: optionalWithDefault<Schema.$Record<Schema.String, Schema.String>>;
    }>>;
}>, typeof CompleteResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the client to the server to ask for completion options.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class Complete extends Complete_base {
}
declare const Root_base: Schema.Class<Root, Schema.Struct<{
    /**
     * The URI identifying the root. This *must* start with file:// for now.
     * This restriction may be relaxed in future versions of the protocol to allow
     * other URI schemes.
     */
    readonly uri: Schema.String;
    /**
     * An optional name for the root. This can be used to provide a human-readable
     * identifier for the root, which may be useful for display purposes or for
     * referencing the root in other parts of the application.
     */
    readonly name: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Optional additional metadata associated with the root.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
}>, {}>;
/**
 * Represents a root directory or file that the server can operate on.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Root extends Root_base {
}
declare const ListRootsResult_base: Schema.Class<ListRootsResult, Schema.Struct<{
    readonly roots: Schema.$Array<typeof Root>;
}>, {}>;
/**
 * Represents a client response containing the roots available to the server.
 *
 * **When to use**
 *
 * Use to return the directories or files that an MCP server may operate on.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ListRootsResult extends ListRootsResult_base {
}
declare const ListRoots_base: Rpc.Rpc<"roots/list", Schema.UndefinedOr<typeof RequestMeta>, typeof ListRootsResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the server to request a list of root URIs from the client. Roots
 * allow servers to ask for specific directories or files to operate on. A
 * common example for roots is providing a set of repositories or directories a
 * server should operate on.
 *
 * **Details**
 *
 * This request is typically used when the server needs to understand the file
 * system structure or access specific locations that the client has permission
 * to read from.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ListRoots extends ListRoots_base {
}
declare const RootsListChangedNotification_base: Rpc.Rpc<"notifications/roots/list_changed", Schema.UndefinedOr<typeof NotificationMeta>, Schema.Void, Schema.Never, never, never>;
/**
 * Represents a notification that the client's root list changed.
 *
 * **When to use**
 *
 * Use to tell the server that it should request an updated roots list.
 *
 * **Details**
 *
 * Send this when the client adds, removes, or modifies a root.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class RootsListChangedNotification extends RootsListChangedNotification_base {
}
declare const ElicitationString_base: Schema.Class<ElicitationString, Schema.Struct<{
    readonly type: Schema.tag<"string">;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly minLength: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly maxLength: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly format: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Literals<readonly ["email", "uri", "date", "date-time"]>>>, Schema.optionalKey<Schema.Literals<readonly ["email", "uri", "date", "date-time"]>>, never, never>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}>;
/**
 * Schema for a string field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class ElicitationString extends ElicitationString_base {
}
declare const ElicitationNumber_base: Schema.Class<ElicitationNumber, Schema.Struct<{
    readonly type: Schema.Literals<readonly ["number", "integer"]>;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly minimum: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    readonly maximum: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
}>, {}>;
/**
 * Schema for a numeric field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class ElicitationNumber extends ElicitationNumber_base {
}
declare const ElicitationBoolean_base: Schema.Class<ElicitationBoolean, Schema.Struct<{
    readonly type: Schema.tag<"boolean">;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, {}>;
/**
 * Schema for a boolean field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class ElicitationBoolean extends ElicitationBoolean_base {
}
declare const UntitledSingleSelectEnum_base: Schema.Class<UntitledSingleSelectEnum, Schema.Struct<{
    readonly type: Schema.tag<"string">;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly enum: Schema.$Array<Schema.String>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}>;
/**
 * Schema for an untitled single-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class UntitledSingleSelectEnum extends UntitledSingleSelectEnum_base {
}
declare const TitledSingleSelectEnum_base: Schema.Class<TitledSingleSelectEnum, Schema.Struct<{
    readonly type: Schema.tag<"string">;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly oneOf: Schema.$Array<Schema.Struct<{
        readonly const: Schema.String;
        readonly title: Schema.String;
    }>>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}>;
/**
 * Schema for a titled single-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class TitledSingleSelectEnum extends TitledSingleSelectEnum_base {
}
/**
 * Schema for every single-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare const SingleSelectEnum: Schema.Union<readonly [typeof UntitledSingleSelectEnum, typeof TitledSingleSelectEnum]>;
/**
 * Type represented by the single-select elicitation field schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type SingleSelectEnum = typeof SingleSelectEnum.Type;
declare const UntitledMultiSelectEnum_base: Schema.Class<UntitledMultiSelectEnum, Schema.Struct<{
    readonly type: Schema.tag<"array">;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly minItems: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly maxItems: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly items: Schema.Struct<{
        readonly type: Schema.tag<"string">;
        readonly enum: Schema.$Array<Schema.String>;
    }>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
}>, {}>;
/**
 * Schema for an untitled multi-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class UntitledMultiSelectEnum extends UntitledMultiSelectEnum_base {
}
declare const TitledMultiSelectEnum_base: Schema.Class<TitledMultiSelectEnum, Schema.Struct<{
    readonly type: Schema.tag<"array">;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly minItems: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly maxItems: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly items: Schema.Struct<{
        readonly anyOf: Schema.$Array<Schema.Struct<{
            readonly const: Schema.String;
            readonly title: Schema.String;
        }>>;
    }>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
}>, {}>;
/**
 * Schema for a titled multi-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class TitledMultiSelectEnum extends TitledMultiSelectEnum_base {
}
/**
 * Schema for every multi-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare const MultiSelectEnum: Schema.Union<readonly [typeof UntitledMultiSelectEnum, typeof TitledMultiSelectEnum]>;
/**
 * Type represented by the multi-select elicitation field schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type MultiSelectEnum = typeof MultiSelectEnum.Type;
declare const LegacyTitledEnum_base: Schema.Class<LegacyTitledEnum, Schema.Struct<{
    readonly type: Schema.tag<"string">;
    readonly title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly enum: Schema.$Array<Schema.String>;
    readonly enumNames: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly default: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>, {}>;
/**
 * Schema for the legacy titled single-select elicitation field.
 *
 * @deprecated Use {@link TitledSingleSelectEnum} instead.
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class LegacyTitledEnum extends LegacyTitledEnum_base {
}
/**
 * Schema for every enumeration field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare const ElicitationEnum: Schema.Union<readonly [typeof LegacyTitledEnum, Schema.Union<readonly [typeof UntitledSingleSelectEnum, typeof TitledSingleSelectEnum]>, Schema.Union<readonly [typeof UntitledMultiSelectEnum, typeof TitledMultiSelectEnum]>]>;
/**
 * Type represented by the elicitation enumeration field schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ElicitationEnum = typeof ElicitationEnum.Type;
/**
 * Schema for primitive field definitions accepted by MCP elicitation forms.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare const PrimitiveSchemaDefinition: Schema.Union<readonly [Schema.Union<readonly [typeof LegacyTitledEnum, Schema.Union<readonly [typeof UntitledSingleSelectEnum, typeof TitledSingleSelectEnum]>, Schema.Union<readonly [typeof UntitledMultiSelectEnum, typeof TitledMultiSelectEnum]>]>, typeof ElicitationString, typeof ElicitationNumber, typeof ElicitationBoolean]>;
/**
 * Type represented by the primitive elicitation field schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type PrimitiveSchemaDefinition = typeof PrimitiveSchemaDefinition.Type;
declare const ElicitRequestFormParams_base: Schema.Class<ElicitRequestFormParams, Schema.Struct<{
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    readonly mode: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Literal<"form">>>, Schema.optionalKey<Schema.Literal<"form">>, never, never>;
    readonly message: Schema.String;
    readonly requestedSchema: Schema.Struct<{
        readonly $schema: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly type: Schema.tag<"object">;
        readonly properties: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.Union<readonly [typeof LegacyTitledEnum, Schema.Union<readonly [typeof UntitledSingleSelectEnum, typeof TitledSingleSelectEnum]>, Schema.Union<readonly [typeof UntitledMultiSelectEnum, typeof TitledMultiSelectEnum]>]>, typeof ElicitationString, typeof ElicitationNumber, typeof ElicitationBoolean]>>;
        readonly required: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    }>;
}>, {}>;
/**
 * Schema for form-mode MCP elicitation requests.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class ElicitRequestFormParams extends ElicitRequestFormParams_base {
}
declare const ElicitRequestURLParams_base: Schema.Class<ElicitRequestURLParams, Schema.Struct<{
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    readonly mode: Schema.Literal<"url">;
    readonly message: Schema.String;
    readonly elicitationId: Schema.String;
    readonly url: Schema.String;
}>, {}>;
/**
 * Schema for URL-mode MCP elicitation requests.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class ElicitRequestURLParams extends ElicitRequestURLParams_base {
}
/**
 * Schema for every MCP elicitation request mode.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare const ElicitRequestParams: Schema.Union<readonly [typeof ElicitRequestFormParams, typeof ElicitRequestURLParams]>;
/**
 * Type represented by the MCP elicitation request parameters schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ElicitRequestParams = typeof ElicitRequestParams.Type;
declare const ElicitAcceptResult_base: Schema.Class<ElicitAcceptResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The user action in response to the elicitation.
     * - "accept": User submitted the form/confirmed the action
     * - "decline": User explicitly declined the action
     * - "cancel": User dismissed without making an explicit choice
     */
    readonly action: Schema.Literal<"accept">;
    /**
     * The submitted form data, only present when action is "accept".
     * Contains values matching the requested schema.
     */
    readonly content: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Finite, Schema.Boolean, Schema.$Array<Schema.String>]>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, Schema.Finite, Schema.Boolean, Schema.$Array<Schema.String>]>>>, never, never>;
}>, {}>;
/**
 * Schema for an accepted client response to an elicitation request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ElicitAcceptResult extends ElicitAcceptResult_base {
}
declare const ElicitDeclineResult_base: Schema.Class<ElicitDeclineResult, Schema.Struct<{
    /**
     * This result property is reserved by the protocol to allow clients and
     * servers to attach additional metadata to their responses.
     */
    readonly _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>>, never, never>;
    /**
     * The user action in response to the elicitation.
     * - "accept": User submitted the form/confirmed the action
     * - "decline": User explicitly declined the action
     * - "cancel": User dismissed without making an explicit choice
     */
    readonly action: Schema.Literals<readonly ["cancel", "decline"]>;
}>, {}>;
/**
 * Schema for a declined or canceled client response to an elicitation request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ElicitDeclineResult extends ElicitDeclineResult_base {
}
/**
 * Schema for every client response to an elicitation request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const ElicitResult: Schema.Union<readonly [typeof ElicitAcceptResult, typeof ElicitDeclineResult]>;
/**
 * Type represented by the MCP elicitation result schema.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ElicitResult = typeof ElicitResult.Type;
/**
 * Version-neutral request for additional MCP client input.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type McpInputRequest = {
    readonly method: "roots/list";
    readonly params?: Schema.JsonObject | undefined;
} | {
    readonly method: "sampling/createMessage";
    readonly params: Schema.JsonObject;
} | {
    readonly method: "elicitation/create";
    readonly params: Schema.JsonObject;
};
/**
 * Version-neutral response supplied by an MCP client for a prior input request.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type McpInputResponse = Schema.JsonObject;
declare const InputRequired_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => Readonly<A> & {
    readonly _tag: "InputRequired";
} & import("../Pipeable.ts").Pipeable;
/**
 * Indicates that an MCP operation requires additional keyed input from the client.
 *
 * **When to use**
 *
 * Use when a handler cannot complete until the client supplies roots, sampling,
 * or elicitation input in a later request.
 *
 * **Details**
 *
 * `requestState` is opaque to the client and is returned unchanged with the
 * keyed input responses.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export declare class InputRequired extends InputRequired_base<{
    readonly inputRequests?: Readonly<Record<string, McpInputRequest>> | undefined;
    readonly requestState?: string | undefined;
}> {
    constructor(fields: {
        readonly inputRequests: Readonly<Record<string, McpInputRequest>>;
        readonly requestState?: string | undefined;
    } | {
        readonly inputRequests?: Readonly<Record<string, McpInputRequest>> | undefined;
        readonly requestState: string;
    });
}
declare const Elicit_base: Rpc.Rpc<"elicitation/create", Schema.Union<readonly [typeof ElicitRequestFormParams, typeof ElicitRequestURLParams]>, Schema.Union<readonly [typeof ElicitAcceptResult, typeof ElicitDeclineResult]>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, never, never>;
/**
 * Sent from the server asking the client to collect structured input from the
 * user.
 *
 * **Details**
 *
 * The client responds with accepted content, an explicit decline, or a
 * cancellation.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class Elicit extends Elicit_base {
}
declare const ElicitationCompleteNotification_base: Rpc.Rpc<"notifications/elicitation/complete", Schema.Struct<{
    elicitationId: Schema.String;
}>, Schema.Void, Schema.Never, never, never>;
/**
 * Notifies a client that a URL-mode elicitation completed.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export declare class ElicitationCompleteNotification extends ElicitationCompleteNotification_base {
}
declare const ElicitationDeclined_base: Schema.Class<ElicitationDeclined, Schema.Struct<{
    readonly _tag: Schema.tag<"ElicitationDeclined">;
    readonly request: Schema.Union<readonly [typeof ElicitRequestFormParams, typeof ElicitRequestURLParams]>;
    readonly cause: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Defect>>, Schema.optionalKey<Schema.Defect>, never, never>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error raised when an MCP elicitation request is declined or fails before
 * accepted content is returned.
 *
 * **Details**
 *
 * The error stores the original elicitation request and, when available, the
 * underlying cause.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class ElicitationDeclined extends ElicitationDeclined_base {
}
declare const McpReverseOperationUnsupported_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "McpReverseOperationUnsupported";
} & Readonly<A>;
/**
 * Raised when the negotiated MCP revision or client capabilities do not
 * support a server-initiated operation.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class McpReverseOperationUnsupported extends McpReverseOperationUnsupported_base<{
    readonly operation: "roots/list" | "sampling/createMessage" | "elicitation/create";
    readonly protocolVersion: ProtocolVersion;
    readonly reason: string;
}> {
}
declare const McpReverseOperationError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "McpReverseOperationError";
} & Readonly<A>;
/**
 * A reverse MCP operation failed while being sent or projected through a
 * version adapter.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class McpReverseOperationError extends McpReverseOperationError_base<{
    readonly operation: "roots/list" | "sampling/createMessage" | "elicitation/create";
    readonly cause: unknown;
}> {
}
/**
 * Version-neutral operations that an MCP server may request from its client.
 *
 * @stability unstable
 * @category client
 * @since 4.0.0
 */
export interface McpReverseClient {
    readonly listRoots: (request?: typeof ListRoots.payloadSchema.Type) => Effect.Effect<ListRootsResult, McpReverseOperationError | McpReverseOperationUnsupported>;
    readonly createMessage: (request: typeof CreateMessage.payloadSchema.Type) => Effect.Effect<CreateMessageResult, McpReverseOperationError | McpReverseOperationUnsupported>;
    readonly elicit: (request: typeof Elicit.payloadSchema.Type) => Effect.Effect<ElicitResult, McpReverseOperationError | McpReverseOperationUnsupported>;
}
declare const McpRequestContext_base: Context.ServiceClass<McpRequestContext, "effect/ai/McpSchema/McpRequestContext", {
    readonly clientId: number;
    readonly protocolVersion: string;
    readonly clientCapabilities: ClientCapabilities;
    readonly clientInfo?: Implementation | undefined;
    readonly requestMetadata?: NonNullable<typeof Initialize.payloadSchema.Type["_meta"]> | Schema.JsonObject | undefined;
    readonly inputResponses?: Readonly<Record<string, McpInputResponse>> | undefined;
    readonly requestState?: string | undefined;
}>;
/**
 * Protocol-neutral context available while handling an MCP request.
 *
 * **Details**
 *
 * Unlike `McpServerClient`, this service does not imply an initialized session
 * or support for server-initiated requests.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class McpRequestContext extends McpRequestContext_base {
}
declare const McpServerClient_base: Context.ServiceClass<McpServerClient, "effect/ai/McpSchema/McpServerClient", {
    readonly clientId: number;
    readonly protocolVersion: StatefulProtocolVersion;
    readonly clientCapabilities: ClientCapabilities;
    readonly clientInfo: Implementation;
    readonly initializePayload: typeof Initialize.payloadSchema["Type"];
    readonly requestMetadata?: Schema.JsonObject | undefined;
    readonly getClient: Effect.Effect<McpReverseClient, never, Scope.Scope>;
}>;
/**
 * Service available while handling an MCP client request.
 *
 * **Details**
 *
 * It exposes the current client id, normalized initialization data, and a
 * scoped version-neutral facade for server-initiated requests.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class McpServerClient extends McpServerClient_base {
}
declare const McpServerClientMiddleware_base: RpcMiddleware.ServiceClass<McpServerClientMiddleware, "effect/ai/McpSchema/McpServerClientMiddleware", McpServerClient, Schema.Never, never, never, false>;
/**
 * RPC middleware that provides `McpServerClient` to handlers for initialized
 * MCP clients.
 *
 * @stability unstable
 * @category middleware
 * @since 4.0.0
 */
export declare class McpServerClientMiddleware extends McpServerClientMiddleware_base {
}
/**
 * Encoded JSON-RPC request message for an RPC in `Group`, including the request
 * id, method, and encoded payload.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type RequestEncoded<Group extends RpcGroup.Any> = RpcGroup.Rpcs<Group> extends infer Rpc ? Rpc extends Rpc.Rpc<infer _Tag, infer _Payload, infer _Success, infer _Error, infer _Middleware> ? {
    readonly _tag: "Request";
    readonly id: string | number;
    readonly method: _Tag;
    readonly payload: _Payload["Encoded"];
} : never : never;
/**
 * Encoded notification message for an RPC in `Group`, including the method and
 * encoded payload without a request id.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type NotificationEncoded<Group extends RpcGroup.Any> = RpcGroup.Rpcs<Group> extends infer Rpc ? Rpc extends Rpc.Rpc<infer _Tag, infer _Payload, infer _Success, infer _Error, infer _Middleware> ? {
    readonly _tag: "Notification";
    readonly method: _Tag;
    readonly payload: _Payload["Encoded"];
} : never : never;
/**
 * Encoded success response for an RPC in `Group`, containing the original
 * request id and encoded result.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type SuccessEncoded<Group extends RpcGroup.Any> = RpcGroup.Rpcs<Group> extends infer Rpc ? Rpc extends Rpc.Rpc<infer _Tag, infer _Payload, infer _Success, infer _Error, infer _Middleware> ? {
    readonly _tag: "Success";
    readonly id: string | number;
    readonly result: _Success["Encoded"];
} : never : never;
/**
 * Encoded failure response for an RPC in `Group`, containing the original
 * request id and encoded error.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type FailureEncoded<Group extends RpcGroup.Any> = RpcGroup.Rpcs<Group> extends infer Rpc ? Rpc extends Rpc.Rpc<infer _Tag, infer _Payload, infer _Success, infer _Error, infer _Middleware> ? {
    readonly _tag: "Failure";
    readonly id: string | number;
    readonly error: _Error["Encoded"];
} : never : never;
declare const ClientRequestRpcs_base: RpcGroup.RpcGroup<Rpc.Rpc<"completion/complete", Schema.Struct<{
    readonly ref: Schema.Union<readonly [typeof PromptReference, typeof ResourceReference]>;
    /**
     * The argument's information
     */
    readonly argument: Schema.Struct<{
        /**
         * The name of the argument
         */
        readonly name: Schema.String;
        /**
         * The value of the argument to use for completion matching.
         */
        readonly value: Schema.String;
    }>;
    /**
     * Additional, optional context for completions
     */
    readonly context: optionalWithDefault<Schema.Struct<{
        /**
         * Previously-resolved variables in a URI template or prompt.
         */
        readonly arguments: optionalWithDefault<Schema.$Record<Schema.String, Schema.String>>;
    }>>;
}>, typeof CompleteResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"initialize", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The latest version of the Model Context Protocol that the client
     * supports. The client MAY decide to support older versions as well.
     */
    protocolVersion: Schema.String;
    /**
     * Capabilities a client may support. Known capabilities are defined here,
     * in this schema, but this is not a closed set: any client can define its
     * own, additional capabilities.
     */
    capabilities: typeof ClientCapabilities;
    /**
     * Describes the name and version of an MCP implementation.
     */
    clientInfo: typeof Implementation;
}>, typeof InitializeResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"logging/setLevel", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The level of logging that the client wants to receive from the server.
     * The server should send all logs at this level and higher (i.e., more
     * severe) to the client as notifications/message.
     */
    level: Schema.Literals<["debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"]>;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"ping", Schema.UndefinedOr<typeof RequestMeta>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"prompts/get", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The name of the prompt or prompt template.
     */
    name: Schema.String;
    title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Arguments to use for templating the prompt.
     */
    arguments: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
}>, typeof GetPromptResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"prompts/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListPromptsResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListResourcesResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/read", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to read. The URI can use any protocol; it is up
     * to the server how to interpret it.
     */
    uri: Schema.String;
}>, typeof ReadResourceResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/subscribe", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/templates/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListResourceTemplatesResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/unsubscribe", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"tools/call", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    name: Schema.String;
    arguments: optionalWithDefault<Schema.$Record<Schema.String, Schema.Any>>;
}>, typeof CallToolResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"tools/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListToolsResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never>>;
/**
 * RPC group for requests that MCP clients send to the server.
 *
 * **Details**
 *
 * The group includes initialization, resource, prompt, tool, logging,
 * completion, and ping requests, and installs `McpServerClientMiddleware` for
 * handlers.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ClientRequestRpcs extends ClientRequestRpcs_base {
}
/**
 * Encoded union of all client-to-server MCP request messages.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ClientRequestEncoded = RequestEncoded<typeof ClientRequestRpcs>;
declare const ClientNotificationRpcs_base: RpcGroup.RpcGroup<typeof InitializedNotification | typeof CancelledNotification | typeof ProgressNotification | typeof RootsListChangedNotification>;
/**
 * RPC group for notifications that MCP clients send to the server, such as
 * cancellation, progress, initialization completion, and roots list changes.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ClientNotificationRpcs extends ClientNotificationRpcs_base {
}
/**
 * Encoded union of all client-to-server MCP notification messages.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ClientNotificationEncoded = NotificationEncoded<typeof ClientNotificationRpcs>;
declare const ClientRpcs_base: RpcGroup.RpcGroup<Rpc.Rpc<"completion/complete", Schema.Struct<{
    readonly ref: Schema.Union<readonly [typeof PromptReference, typeof ResourceReference]>;
    /**
     * The argument's information
     */
    readonly argument: Schema.Struct<{
        /**
         * The name of the argument
         */
        readonly name: Schema.String;
        /**
         * The value of the argument to use for completion matching.
         */
        readonly value: Schema.String;
    }>;
    /**
     * Additional, optional context for completions
     */
    readonly context: optionalWithDefault<Schema.Struct<{
        /**
         * Previously-resolved variables in a URI template or prompt.
         */
        readonly arguments: optionalWithDefault<Schema.$Record<Schema.String, Schema.String>>;
    }>>;
}>, typeof CompleteResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"initialize", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The latest version of the Model Context Protocol that the client
     * supports. The client MAY decide to support older versions as well.
     */
    protocolVersion: Schema.String;
    /**
     * Capabilities a client may support. Known capabilities are defined here,
     * in this schema, but this is not a closed set: any client can define its
     * own, additional capabilities.
     */
    capabilities: typeof ClientCapabilities;
    /**
     * Describes the name and version of an MCP implementation.
     */
    clientInfo: typeof Implementation;
}>, typeof InitializeResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"logging/setLevel", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The level of logging that the client wants to receive from the server.
     * The server should send all logs at this level and higher (i.e., more
     * severe) to the client as notifications/message.
     */
    level: Schema.Literals<["debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"]>;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"ping", Schema.UndefinedOr<typeof RequestMeta>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"prompts/get", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The name of the prompt or prompt template.
     */
    name: Schema.String;
    title: Schema.decodeTo<Schema.toType<Schema.optional<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    /**
     * Arguments to use for templating the prompt.
     */
    arguments: Schema.decodeTo<Schema.toType<Schema.optional<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
}>, typeof GetPromptResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"prompts/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListPromptsResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListResourcesResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/read", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to read. The URI can use any protocol; it is up
     * to the server how to interpret it.
     */
    uri: Schema.String;
}>, typeof ReadResourceResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/subscribe", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/templates/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListResourceTemplatesResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"resources/unsubscribe", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String;
}>, Schema.Struct<{}>, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"tools/call", Schema.Struct<{
    _meta: Schema.decodeTo<Schema.toType<Schema.optional<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>>, Schema.optionalKey<Schema.StructWithRest<Schema.Struct<{
        /**
         * If specified, the caller is requesting out-of-band progress notifications
         * for this request (as represented by notifications/progress). The value of
         * this parameter is an opaque token that will be attached to any subsequent
         * notifications. The receiver is not obligated to provide these
         * notifications.
         */
        readonly progressToken: Schema.decodeTo<Schema.toType<Schema.optional<Schema.Union<[Schema.String, Schema.Finite]>>>, Schema.optionalKey<Schema.Union<[Schema.String, Schema.Finite]>>, never, never>;
    }>, readonly [Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>]>>, never, never>;
    name: Schema.String;
    arguments: optionalWithDefault<Schema.$Record<Schema.String, Schema.Any>>;
}>, typeof CallToolResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | Rpc.Rpc<"tools/list", Schema.UndefinedOr<typeof PaginatedRequestMeta>, typeof ListToolsResult, Schema.Union<readonly [typeof ParseError, typeof InvalidRequest, typeof MethodNotFound, typeof InvalidParams, typeof InternalError, typeof McpErrorBase]>, typeof McpServerClientMiddleware, never> | typeof InitializedNotification | typeof CancelledNotification | typeof ProgressNotification | typeof RootsListChangedNotification>;
/**
 * RPC group combining all client-to-server MCP requests and notifications.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ClientRpcs extends ClientRpcs_base {
}
/**
 * Encoded success response sent by a client for a server-initiated request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ClientSuccessEncoded = SuccessEncoded<typeof ServerRequestRpcs>;
/**
 * Encoded failure response sent by a client for a server-initiated request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ClientFailureEncoded = FailureEncoded<typeof ServerRequestRpcs>;
declare const ServerRequestRpcs_base: RpcGroup.RpcGroup<typeof Ping | typeof CreateMessage | typeof ListRoots | typeof Elicit>;
/**
 * RPC group for requests that an MCP server can send to a client, including
 * ping, sampling, roots listing, and elicitation.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ServerRequestRpcs extends ServerRequestRpcs_base {
}
/**
 * Encoded union of all server-to-client MCP request messages.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ServerRequestEncoded = RequestEncoded<typeof ServerRequestRpcs>;
declare const ServerNotificationRpcs_base: RpcGroup.RpcGroup<typeof CancelledNotification | typeof ProgressNotification | typeof ResourceListChangedNotification | typeof ResourceUpdatedNotification | typeof PromptListChangedNotification | typeof ToolListChangedNotification | typeof LoggingMessageNotification | typeof ElicitationCompleteNotification>;
/**
 * RPC group for notifications that an MCP server can send to a client,
 * including cancellation, progress, logging, and list or resource update
 * notifications.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export declare class ServerNotificationRpcs extends ServerNotificationRpcs_base {
}
/**
 * Encoded union of all server-to-client MCP notification messages.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ServerNotificationEncoded = NotificationEncoded<typeof ServerNotificationRpcs>;
/**
 * Encoded success response sent by the server for a client-initiated request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ServerSuccessEncoded = SuccessEncoded<typeof ClientRequestRpcs>;
/**
 * Encoded failure response sent by the server for a client-initiated request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ServerFailureEncoded = FailureEncoded<typeof ClientRequestRpcs>;
/**
 * Encoded server response to a client request, either success or failure.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type ServerResultEncoded = ServerSuccessEncoded | ServerFailureEncoded;
/**
 * Encoded MCP messages accepted from a client by the server protocol: client
 * requests and client notifications.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type FromClientEncoded = ClientRequestEncoded | ClientNotificationEncoded;
/**
 * Encoded MCP messages emitted by the server protocol to a client: server
 * responses and server notifications.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export type FromServerEncoded = ServerResultEncoded | ServerNotificationEncoded;
declare const ParamSchemaTypeId = "~effect/ai/McpSchema/ParamSchema";
/**
 * Returns `true` when a schema was created with `param` and therefore carries
 * a resource URI template parameter name.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare function isParam(schema: Schema.Constraint): schema is Param<string, Schema.Top>;
/**
 * Schema wrapper used for resource URI template parameters.
 *
 * **Details**
 *
 * A `Param` behaves like the wrapped schema while carrying the parameter name
 * used for template compilation and completion lookup.
 *
 * @stability unstable
 * @category parameters
 * @since 4.0.0
 */
export interface Param<Name extends string, S extends Schema.Constraint> extends Schema.BottomLazy<S["ast"], Param<Name, S>, S["~type.parameters"], S["~type.mutability"], S["~type.optionality"], S["~type.constructor.default"], S["~encoded.mutability"], S["~encoded.optionality"]> {
    readonly "Type": S["Type"];
    readonly "Encoded": S["Encoded"];
    readonly "DecodingServices": S["DecodingServices"];
    readonly "EncodingServices": S["EncodingServices"];
    readonly "Rebuild": Param<Name, S>;
    readonly "~type.make.in": S["~type.make.in"];
    readonly "~type.make": S["~type.make"];
    readonly "Iso": S["Iso"];
    readonly [ParamSchemaTypeId]: typeof ParamSchemaTypeId;
    readonly name: Name;
    readonly schema: S;
}
/**
 * Creates a parameter for a resource URI template.
 *
 * @stability unstable
 * @category parameters
 * @since 4.0.0
 */
export declare function param<const Name extends string, S extends Schema.Constraint>(name: Name, schema: S): Param<Name, S>;
declare const EnabledWhen_base: Context.ServiceClass<EnabledWhen, "effect/ai/McpSchema/EnabledWhen", Predicate.Predicate<{
    readonly protocolVersion: string;
    readonly capabilities: ClientCapabilities;
    readonly clientInfo?: Implementation | undefined;
}>>;
/**
 * Annotation to conditionally enable or disable tools based on client
 * information.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class EnabledWhen extends EnabledWhen_base {
}
export {};
//# sourceMappingURL=McpSchema.d.ts.map