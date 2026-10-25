/**
 * @since 2.0.0
 */
/** @internal */
export const isArrayNonEmpty = self => self.length > 0;
/** @internal */
export function replaceAt(self, index, value) {
  const out = self.slice();
  out[index] = value;
  return out;
}
//# sourceMappingURL=array.js.map