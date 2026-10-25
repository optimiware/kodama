/**
 * @internal
 */
export function normalize(contentType) {
  const normalized = contentType.toLowerCase().trim();
  const index = normalized.indexOf(";");
  return index === -1 ? normalized : normalized.slice(0, index).trim();
}
//# sourceMappingURL=mediaType.js.map