import { protocol as protocol2024_11_05 } from "./internal/mcpProtocol/v2024_11_05.js";
import { protocol as protocol2025_03_26 } from "./internal/mcpProtocol/v2025_03_26.js";
import { protocol as protocol2025_06_18 } from "./internal/mcpProtocol/v2025_06_18.js";
import { protocol as protocol2025_11_25 } from "./internal/mcpProtocol/v2025_11_25.js";
import { protocol as protocol2026_07_28 } from "./internal/mcpProtocol/v2026_07_28.js";
/**
 * The MCP 2026-07-28 protocol implementation.
 *
 * **Details**
 *
 * This revision uses request-scoped metadata instead of initialization and
 * protocol-level sessions.
 *
 * When the selected transport supports server notifications, discovery
 * advertises change-notification capabilities and `subscriptions/listen`
 * delivers the requested notifications over the long-lived response.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export const v2026_07_28 = protocol2026_07_28;
/**
 * The MCP 2025-11-25 protocol implementation.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export const v2025_11_25 = protocol2025_11_25;
/**
 * The MCP 2025-06-18 protocol implementation.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export const v2025_06_18 = protocol2025_06_18;
/**
 * The MCP 2025-03-26 protocol implementation.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export const v2025_03_26 = protocol2025_03_26;
/**
 * The MCP 2024-11-05 protocol implementation.
 *
 * **Details**
 *
 * It provides the dated schema and stdio behavior. When supplied to
 * `McpServer.layerHttp`, the server uses its single-endpoint Streamable HTTP
 * compatibility transport; it does not implement the historical two-endpoint
 * HTTP+SSE transport.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export const v2024_11_05 = protocol2024_11_05;
//# sourceMappingURL=McpProtocol.js.map