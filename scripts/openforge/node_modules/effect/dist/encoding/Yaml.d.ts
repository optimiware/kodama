/**
 * Parses YAML configuration files.
 *
 * Supports YAML 1.2 block and flow collections, indentless sequence values,
 * multiline plain and quoted scalars, block scalars, anchors, and aliases.
 *
 * Unsupported: compact nested block sequences (`- - value` on one line),
 * multiline flow collections, explicit complex keys, tag/directive processing,
 * recursive aliases, and document streams. Mapping keys are JavaScript strings.
 *
 * @stability unstable
 * @since 4.0.0
 */
/**
 * Parses one YAML document into JavaScript values.
 *
 * **Details**
 *
 * The core YAML 1.2 scalar schema is used, so booleans, nulls, and numbers are
 * decoded while date-like values remain strings.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const parse: (input: string) => unknown;
//# sourceMappingURL=Yaml.d.ts.map