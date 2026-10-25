export * as Connection from "./connection.js";
import { Schema } from "effect";
/**
 * Runtime problem an integration reported for a connection. `needs_auth` asks the user to sign in again:
 * at `url` when set (the credential itself stays valid), otherwise by reconnecting the integration.
 */
export interface Status extends Schema.Schema.Type<typeof Status> {
}
export declare const Status: Schema.Struct<{
    readonly status: Schema.Literals<readonly ["needs_auth"]>;
    readonly message: Schema.String;
    readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface CredentialInfo extends Schema.Schema.Type<typeof CredentialInfo> {
}
export declare const CredentialInfo: Schema.Struct<{
    readonly type: Schema.Literal<"credential">;
    readonly id: Schema.brand<Schema.String, "Credential.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Credential.ID">;
    };
    readonly label: Schema.String;
    /** Whether the connection stores a key, an OAuth grant, or an external credential-source reference. */
    readonly method: Schema.Literals<readonly ["key", "oauth", "external"]>;
    readonly status: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
}>;
export interface EnvInfo extends Schema.Schema.Type<typeof EnvInfo> {
}
export declare const EnvInfo: Schema.Struct<{
    readonly type: Schema.Literal<"env">;
    readonly name: Schema.String;
    readonly status: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
}>;
export declare const Info: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"credential">;
    readonly id: Schema.brand<Schema.String, "Credential.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Credential.ID">;
    };
    readonly label: Schema.String;
    /** Whether the connection stores a key, an OAuth grant, or an external credential-source reference. */
    readonly method: Schema.Literals<readonly ["key", "oauth", "external"]>;
    readonly status: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"env">;
    readonly name: Schema.String;
    readonly status: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly status: Schema.Literals<readonly ["needs_auth"]>;
        readonly message: Schema.String;
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
}>]>;
export type Info = typeof Info.Type;
