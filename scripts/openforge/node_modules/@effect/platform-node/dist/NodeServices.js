import * as Layer from "effect/Layer";
import * as NodeChildProcessSpawner from "./NodeChildProcessSpawner.js";
import * as NodeCrypto from "./NodeCrypto.js";
import * as NodeFileSystem from "./NodeFileSystem.js";
import * as NodePath from "./NodePath.js";
import * as NodeStdio from "./NodeStdio.js";
import * as NodeTerminal from "./NodeTerminal.js";
/**
 * Provides the default Node implementations for child process spawning,
 * filesystem, path, stdio, and terminal services.
 *
 * @category layers
 * @since 4.0.0
 */
export const layer = /*#__PURE__*/Layer.provideMerge(NodeChildProcessSpawner.layer, /*#__PURE__*/Layer.mergeAll(NodeFileSystem.layer, NodeCrypto.layer, NodePath.layer, NodeStdio.layer, NodeTerminal.layer));
//# sourceMappingURL=NodeServices.js.map