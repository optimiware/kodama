import type * as Layer from "effect/Layer";
import type { Path } from "effect/Path";
/**
 * Provides the default Node `Path` service using the platform's `node:path`
 * implementation.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<Path>;
/**
 * Provides the `Path` service using Node's POSIX path implementation,
 * regardless of the host platform.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layerPosix: Layer.Layer<Path>;
/**
 * Provides the `Path` service using Node's Windows path implementation,
 * regardless of the host platform.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layerWin32: Layer.Layer<Path>;
//# sourceMappingURL=NodePath.d.ts.map