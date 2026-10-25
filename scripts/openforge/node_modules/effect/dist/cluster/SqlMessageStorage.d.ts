import * as Crypto from "../Crypto.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import * as SqlClient from "../sql/SqlClient.ts";
import * as MessageStorage from "./MessageStorage.ts";
import type { ShardingConfig } from "./ShardingConfig.ts";
import * as Snowflake from "./Snowflake.ts";
/**
 * Creates a SQL-backed encoded message storage driver, running its migrations
 * and using the optional table prefix.
 *
 * **When to use**
 *
 * Use when you need the SQL-backed encoded driver directly, such as when
 * composing a custom message storage adapter.
 *
 * **Details**
 *
 * The optional `prefix` controls the table names for messages, replies, and
 * migrations; when omitted, `cluster` is used.
 *
 * **Gotchas**
 *
 * Changing `prefix` after deployment points the runtime at a different set of
 * tables, including the migration history table.
 *
 * @see {@link make} for the decoded `MessageStorage` constructor
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeEncoded: (options?: {
    readonly prefix?: string | undefined;
}) => Effect.Effect<MessageStorage.Encoded, never, SqlClient.SqlClient | Crypto.Crypto>;
/**
 * Creates a SQL-backed `MessageStorage` implementation, running its migrations
 * and using the optional table prefix.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (options?: {
    readonly prefix?: string | undefined;
}) => Effect.Effect<MessageStorage.MessageStorage["Service"], never, SqlClient.SqlClient | Snowflake.Generator | Crypto.Crypto>;
/**
 * Layer that provides SQL-backed `MessageStorage` using the default table prefix
 * and the default snowflake generator.
 *
 * **When to use**
 *
 * Use when a cluster should persist mailbox messages and replies in SQL using
 * the default `cluster` table prefix and the standard snowflake generator.
 *
 * **Details**
 *
 * The layer runs the SQL migrations through `make`, provides `MessageStorage`,
 * and supplies `Snowflake.layerGenerator` internally. Callers still provide
 * `SqlClient`, `ShardingConfig`, and `Crypto.Crypto`, which is used to hash
 * message deduplication keys that would overflow the fixed-width
 * `message_id` column.
 *
 * **Gotchas**
 *
 * This layer always uses the `cluster` table prefix. Use `layerWith` before
 * deployment if you need a different stable prefix, because changing prefixes
 * later points the runtime at a different set of tables.
 *
 * @see {@link layerWith} for the same SQL storage layer with a custom table prefix
 * @see {@link make} for the lower-level service constructor that uses an existing `Snowflake.Generator`
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<MessageStorage.MessageStorage, never, SqlClient.SqlClient | ShardingConfig | Crypto.Crypto>;
/**
 * Layer that provides SQL-backed `MessageStorage` using a custom table prefix.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerWith: (options: {
    readonly prefix?: string | undefined;
}) => Layer.Layer<MessageStorage.MessageStorage, never, SqlClient.SqlClient | ShardingConfig | Crypto.Crypto>;
//# sourceMappingURL=SqlMessageStorage.d.ts.map