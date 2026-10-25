import { Schema } from "effect";
/**
 * Host-supplied session annotations, durable and opaque to core. Keys are
 * arbitrary; values must be JSON-serializable. Children and forks inherit
 * the parent's current metadata unless the creator supplies its own.
 */
export declare const SessionMetadata: Schema.$Record<Schema.String, Schema.Codec<Schema.Json, Schema.Json, never, never>>;
export type SessionMetadata = typeof SessionMetadata.Type;
