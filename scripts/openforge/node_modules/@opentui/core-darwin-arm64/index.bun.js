const module = await import("./libopentui.dylib", { with: { type: "file" } })

export default module.default
