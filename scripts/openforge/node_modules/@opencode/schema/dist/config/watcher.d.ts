export * as ConfigWatcher from "./watcher.js";
import { Schema } from "effect";
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly ignore: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
