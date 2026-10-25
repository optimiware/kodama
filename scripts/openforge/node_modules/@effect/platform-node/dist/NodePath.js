/**
 * Node.js layers for Effect's `Path` service.
 *
 * This module provides the default, POSIX, and Windows variants of the
 * platform-independent `Path` service by reusing the shared Node path
 * implementation. The provided path services include Node file URL conversion
 * behavior.
 *
 * @since 4.0.0
 */
import * as NodePath from "@effect/platform-node-shared/NodePath";
/**
 * Provides the default Node `Path` service using the platform's `node:path`
 * implementation.
 *
 * @category layers
 * @since 4.0.0
 */
export const layer = NodePath.layer;
/**
 * Provides the `Path` service using Node's POSIX path implementation,
 * regardless of the host platform.
 *
 * @category layers
 * @since 4.0.0
 */
export const layerPosix = NodePath.layerPosix;
/**
 * Provides the `Path` service using Node's Windows path implementation,
 * regardless of the host platform.
 *
 * @category layers
 * @since 4.0.0
 */
export const layerWin32 = NodePath.layerWin32;
//# sourceMappingURL=NodePath.js.map