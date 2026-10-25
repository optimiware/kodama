import { Schema } from "effect";
export declare const ServerOptions: Schema.Struct<{
    readonly app: Schema.optional<Schema.Struct<{
        readonly name: Schema.optional<Schema.String>;
        readonly version: Schema.optional<Schema.String>;
        readonly channel: Schema.optional<Schema.String>;
    }>>;
    readonly hostname: Schema.optional<Schema.String>;
    readonly port: Schema.optional<Schema.Int>;
    readonly password: Schema.optional<Schema.String>;
    readonly cors: Schema.optional<Schema.$Array<Schema.String>>;
    readonly simulation: Schema.optional<Schema.Boolean>;
    readonly database: Schema.optional<Schema.Struct<{
        readonly path: Schema.optional<Schema.String>;
    }>>;
    readonly pty: Schema.optional<Schema.Struct<{
        readonly handoff: Schema.optional<Schema.Struct<{
            readonly directory: Schema.String;
            readonly instanceID: Schema.String;
            readonly ticket: Schema.String;
            readonly expiresAt: Schema.Number;
        }>>;
        readonly root: Schema.optional<Schema.String>;
    }>>;
    readonly events: Schema.optional<Schema.Struct<{
        readonly persist: Schema.optional<Schema.Boolean>;
    }>>;
    readonly models: Schema.optional<Schema.Struct<{
        readonly url: Schema.optional<Schema.String>;
        readonly file: Schema.optional<Schema.String>;
        readonly fetch: Schema.optional<Schema.Boolean>;
        readonly snapshot: Schema.optional<Schema.Boolean>;
    }>>;
    readonly config: Schema.optional<Schema.Struct<{
        readonly directory: Schema.optional<Schema.String>;
        readonly project: Schema.optional<Schema.Boolean>;
        readonly claude: Schema.optional<Schema.Boolean>;
        readonly file: Schema.optional<Schema.String>;
        readonly content: Schema.optional<Schema.String>;
    }>>;
    readonly windows: Schema.optional<Schema.Struct<{
        readonly gitbash: Schema.optional<Schema.String>;
    }>>;
    readonly fs: Schema.optional<Schema.Struct<{
        readonly filewatcher: Schema.optional<Schema.Boolean>;
        readonly fff: Schema.optional<Schema.Boolean>;
    }>>;
}>;
export type ServerOptions = typeof ServerOptions.Type;
