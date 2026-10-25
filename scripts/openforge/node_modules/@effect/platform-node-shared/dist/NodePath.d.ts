import * as Layer from "effect/Layer";
import { Path } from "effect/Path";
/**
 * Provides the `Path` service using Node's POSIX path implementation plus
 * file URL conversion helpers.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layerPosix: Layer.Layer<Path>;
/**
 * Provides the `Path` service using Node's Windows path implementation plus
 * file URL conversion helpers.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layerWin32: Layer.Layer<Path>;
/**
 * Provides the default `Path` service using the host platform's Node path
 * implementation plus file URL conversion helpers.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<Path>;
//# sourceMappingURL=NodePath.d.ts.map