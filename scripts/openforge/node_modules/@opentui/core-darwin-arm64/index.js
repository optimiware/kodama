import { fileURLToPath } from "node:url"

export default fileURLToPath(new URL("./libopentui.dylib", import.meta.url))
