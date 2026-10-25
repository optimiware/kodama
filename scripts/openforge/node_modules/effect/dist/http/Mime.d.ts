/**
 * Standard MIME type lookup.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Option from "../Option.ts";
/**
 * Returns the standard MIME type associated with a file name or extension.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export declare const getType: (path: string) => Option.Option<string>;
/**
 * Returns the default file extension associated with a standard MIME type.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export declare const getExtension: (type: string) => Option.Option<string>;
/**
 * Returns every file extension associated with a standard MIME type.
 *
 * @stability unstable
 * @category utilities
 * @since 4.0.0
 */
export declare const getAllExtensions: (type: string) => Option.Option<ReadonlySet<string>>;
//# sourceMappingURL=Mime.d.ts.map