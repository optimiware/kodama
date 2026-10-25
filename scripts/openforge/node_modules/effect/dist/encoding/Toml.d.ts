/**
 * Parses TOML configuration files.
 *
 * The implementation covers the TOML values and table forms used by Effect's
 * configuration-file CLI primitive while avoiding a generated parser runtime.
 *
 * @stability unstable
 * @since 4.0.0
 */
/**
 * Parses a TOML document into a null-prototype record.
 *
 * **Details**
 *
 * Offset date-times are represented by `Date`, matching the package this
 * parser replaces. Local dates and times remain strings.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const parse: (input: string) => Record<string, unknown>;
//# sourceMappingURL=Toml.d.ts.map