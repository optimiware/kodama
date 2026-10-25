import * as Layer from "effect/Layer";
import * as Stdio from "effect/Stdio";
/**
 * Provides `Stdio` from `process.argv`, `process.stdin`, `process.stdout`,
 * and `process.stderr`; stdin remains open and stdout/stderr are not ended by
 * default.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<Stdio.Stdio>;
//# sourceMappingURL=NodeStdio.d.ts.map