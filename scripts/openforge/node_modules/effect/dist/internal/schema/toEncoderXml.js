import * as Effect from "../../Effect.js";
import { format } from "../../Formatter.js";
import * as SchemaParser from "../../SchemaParser.js";
import * as InternalAnnotations from "./annotations.js";
import * as InternalToCodec from "./toCodec.js";
/** @internal */
export function toEncoderXml(codec, options) {
  const rootName = InternalAnnotations.resolveIdentifier(codec.ast) ?? InternalAnnotations.resolveTitle(codec.ast);
  const serialize = SchemaParser.encodeEffect(InternalToCodec.toCodecStringTree(codec));
  return value => serialize(value).pipe(Effect.map(stringTree => stringTreeToXml(stringTree, {
    rootName,
    ...options
  })));
}
function stringTreeToXml(value, options) {
  const rootName = options.rootName ?? "root";
  const arrayItemName = options.arrayItemName ?? "item";
  const pretty = options.pretty ?? true;
  const indent = options.indent ?? "  ";
  const sortKeys = options.sortKeys ?? true;
  const seen = new Set();
  const lines = [];
  recur(rootName, value, 0);
  return lines.join(pretty ? "\n" : "");
  function push(depth, text) {
    lines.push(pretty ? indent.repeat(depth) + text : text);
  }
  function recur(tagName, node, depth, originalNameForMeta) {
    const {
      attrs,
      safe
    } = xml.tagInfo(tagName, originalNameForMeta);
    if (node === undefined) {
      push(depth, `<${safe}${attrs}/>`);
    } else if (typeof node === "string") {
      push(depth, `<${safe}${attrs}>${xml.escapeText(node)}</${safe}>`);
    } else if (typeof node !== "object" || node === null) {
      push(depth, `<${safe}${attrs}>${xml.escapeText(format(node))}</${safe}>`);
    } else {
      if (seen.has(node)) throw new globalThis.Error("Cycle detected while serializing to XML.", {
        cause: node
      });
      seen.add(node);
      try {
        if (globalThis.Array.isArray(node)) {
          if (node.length === 0) {
            push(depth, `<${safe}${attrs}/>`);
            return;
          }
          push(depth, `<${safe}${attrs}>`);
          for (const item of node) recur(arrayItemName, item, depth + 1);
          push(depth, `</${safe}>`);
          return;
        }
        const object = node;
        const keys = Object.keys(object);
        if (sortKeys) keys.sort();
        if (keys.length === 0) {
          push(depth, `<${safe}${attrs}/>`);
          return;
        }
        push(depth, `<${safe}${attrs}>`);
        for (const key of keys) {
          recur(xml.parseTagName(key).safe, object[key], depth + 1, key);
        }
        push(depth, `</${safe}>`);
      } finally {
        seen.delete(node);
      }
    }
  }
}
const xml = {
  escapeText(value) {
    return value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  },
  escapeAttribute(value) {
    return value.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  },
  parseTagName(name) {
    const original = name;
    let safe = name;
    if (!/^[A-Za-z_]/.test(safe)) safe = "_" + safe;
    safe = safe.replace(/[^A-Za-z0-9._-]/g, "_");
    if (/^xml/i.test(safe)) safe = "_" + safe;
    return {
      safe,
      changed: safe !== original
    };
  },
  tagInfo(name, original) {
    const {
      changed,
      safe
    } = xml.parseTagName(name);
    const needsMeta = changed || original && original !== name;
    const attrs = needsMeta ? ` data-name="${xml.escapeAttribute(original ?? name)}"` : "";
    return {
      safe,
      attrs
    };
  }
};
//# sourceMappingURL=toEncoderXml.js.map