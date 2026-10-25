import * as internal from "./MultipartParser/internal/multipart.js";
/**
 * Creates a streaming multipart parser.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = internal.make;
/**
 * Determines whether a multipart part should be treated as a file.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export const defaultIsFile = internal.defaultIsFile;
/**
 * Decodes a multipart field using its declared character set.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export const decodeField = internal.decodeField;
//# sourceMappingURL=MultipartParser.js.map