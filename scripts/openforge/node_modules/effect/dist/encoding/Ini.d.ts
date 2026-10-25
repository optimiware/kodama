/**
 * Parses INI configuration files.
 *
 * This module contains the decoding surface used by Effect's CLI without
 * pulling in the complete `ini` package.
 *
 * @stability unstable
 * @since 4.0.0
 */
/**
 * Parses an INI document into a null-prototype record.
 *
 * **Details**
 *
 * Section names separated by dots become nested records, repeated keys ending
 * in `[]` become arrays, and the scalar values `true`, `false`, and `null` are
 * decoded. Other scalar values remain strings.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const parse: (input: string) => Record<string, unknown>;
//# sourceMappingURL=Ini.d.ts.map