export * as ConfigMedia from "./media.js";
import { Schema } from "effect";
declare const Image_base: Schema.Class<Image, Schema.Struct<{
    readonly auto_resize: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly max_width: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly max_height: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly max_base64_bytes: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>, {}>;
export declare class Image extends Image_base {
}
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly image: Schema.decodeTo<Schema.optional<Schema.toType<typeof Image>>, Schema.optionalKey<typeof Image>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
