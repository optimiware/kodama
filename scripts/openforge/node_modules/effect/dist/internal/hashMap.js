/**
 * @since 2.0.0
 */
import * as Equal_ from "../Equal.js";
import { format } from "../Formatter.js";
import { dual, identity, pipe } from "../Function.js";
import * as Hash from "../Hash.js";
import { NodeInspectSymbol, toJson } from "../Inspectable.js";
import * as Option from "../Option.js";
import { pipeArguments } from "../Pipeable.js";
import { hasProperty } from "../Predicate.js";
import * as Result from "../Result.js";
import { backEdges } from "./hash.js";
/** @internal */
export const HashMapTypeId = "~effect/HashMap";
// HAMT Implementation
/** @internal */
const SHIFT = 5;
/** @internal */
const BUCKET_SIZE = 1 << SHIFT; // 32
/** @internal */
// const BITMAP_SIZE = 1 << SHIFT // 32
/** @internal */
const MIN_ARRAY_NODE = BUCKET_SIZE / 4; // 8
/** @internal */
const MAX_INDEX_NODE = BUCKET_SIZE / 2; // 16
/** @internal */
const BITMAP_INDEX_MASK = BUCKET_SIZE - 1; // 31
/** @internal */
const popcount = n => {
  n = n - (n >>> 1 & 0x55555555);
  n = (n & 0x33333333) + (n >>> 2 & 0x33333333);
  return (n + (n >>> 4) & 0xF0F0F0F) * 0x1010101 >>> 24;
};
/** @internal */
const mask = (hash, shift) => hash >>> shift & BITMAP_INDEX_MASK;
/** @internal */
const bitpos = (hash, shift) => 1 << mask(hash, shift);
/** @internal */
const index = (bitmap, bit) => popcount(bitmap & bit - 1);
/** @internal */
function mergeLeaves(edit, shift, hash1, node1, hash2, node2) {
  if (shift > 32) {
    throw new Error("HashMap: max depth exceeded");
  }
  const bit1 = bitpos(hash1, shift);
  const bit2 = bitpos(hash2, shift);
  if (bit1 === bit2) {
    const child = mergeLeaves(edit, shift + SHIFT, hash1, node1, hash2, node2);
    return new IndexedNode(edit, bit1, [child]);
  }
  const bitmap = bit1 | bit2;
  const children = bit1 >>> 0 < bit2 >>> 0 ? [node1, node2] : [node2, node1];
  return new IndexedNode(edit, bitmap, children);
}
const hashMapSeed = /*#__PURE__*/Hash.string("HashMap");
const entryHash = (key, value) => Hash.combine(Hash.hash(key), Hash.hash(value));
const childrenHash = children => {
  let h = 0;
  for (let i = 0; i < children.length; i++) {
    const child = children[i];
    if (child) h ^= child.subtreeHash();
  }
  return h;
};
/** @internal */
class Node {
  canEdit(edit) {
    return this.edit === edit;
  }
  subtreeHash() {
    let h = this._hash;
    if (h === undefined) {
      const seen = backEdges;
      h = this.computeHash();
      // Cycle hashes depend on the entry point.
      if (seen === backEdges) {
        this._hash = h;
      }
    }
    return h;
  }
}
/** @internal */
class EmptyNode extends Node {
  _tag = "EmptyNode";
  edit = 0;
  get size() {
    return 0;
  }
  get(_shift, _hash, _key) {
    return Option.none();
  }
  has(_shift, _hash, _key) {
    return false;
  }
  set(edit, _shift, hash, key, value, added) {
    added.value = true;
    return new LeafNode(edit, hash, key, value);
  }
  remove(_edit, _shift, _hash, _key, _removed) {
    return this;
  }
  canEdit(_edit) {
    return false;
  }
  computeHash() {
    return 0;
  }
}
/** @internal */
class LeafNode extends Node {
  _tag = "LeafNode";
  edit;
  hash;
  key;
  value;
  constructor(edit, hash, key, value) {
    super();
    this.edit = edit;
    this.hash = hash;
    this.key = key;
    this.value = value;
  }
  get size() {
    return 1;
  }
  get(_shift, hash, key) {
    if (this.hash === hash && Equal_.equals(this.key, key)) {
      return Option.some(this.value);
    }
    return Option.none();
  }
  has(_shift, hash, key) {
    return this.hash === hash && Equal_.equals(this.key, key);
  }
  set(edit, shift, hash, key, value, added) {
    if (this.canEdit(edit)) this._hash = undefined;
    if (this.hash === hash && Equal_.equals(this.key, key)) {
      if (Equal_.equals(this.value, value)) {
        return this;
      }
      // Can mutate in-place if edit matches
      if (this.canEdit(edit)) {
        this.value = value;
        return this;
      }
      return new LeafNode(edit, hash, key, value);
    }
    added.value = true;
    if (this.hash === hash) {
      return new CollisionNode(edit, hash, [[this.key, this.value], [key, value]]);
    }
    const newBit = bitpos(hash, shift);
    const existingBit = bitpos(this.hash, shift);
    if (newBit === existingBit) {
      return new IndexedNode(edit, newBit, [this.set(edit, shift + SHIFT, hash, key, value, added)]);
    }
    const bitmap = newBit | existingBit;
    const nodes = newBit >>> 0 < existingBit >>> 0 ? [new LeafNode(edit, hash, key, value), this] : [this, new LeafNode(edit, hash, key, value)];
    return new IndexedNode(edit, bitmap, nodes);
  }
  remove(_edit, _shift, hash, key, removed) {
    if (this.hash === hash && Equal_.equals(this.key, key)) {
      removed.value = true;
      return undefined;
    }
    return this;
  }
  computeHash() {
    // `setHash` allows the stored hash to differ from `Hash.hash(key)`.
    return entryHash(this.key, this.value);
  }
}
/** @internal */
class CollisionNode extends Node {
  _tag = "CollisionNode";
  edit;
  hash;
  entries;
  constructor(edit, hash, entries) {
    super();
    this.edit = edit;
    this.hash = hash;
    this.entries = entries;
  }
  get size() {
    return this.entries.length;
  }
  get(_shift, hash, key) {
    if (this.hash !== hash) {
      return Option.none();
    }
    for (const [k, v] of this.entries) {
      if (Equal_.equals(k, key)) {
        return Option.some(v);
      }
    }
    return Option.none();
  }
  has(_shift, hash, key) {
    if (this.hash !== hash) {
      return false;
    }
    for (const [k] of this.entries) {
      if (Equal_.equals(k, key)) {
        return true;
      }
    }
    return false;
  }
  set(edit, shift, hash, key, value, added) {
    if (this.canEdit(edit)) this._hash = undefined;
    if (this.hash !== hash) {
      added.value = true;
      // Need to merge this collision node with new leaf
      return mergeLeaves(edit, shift, this.hash, this, hash, new LeafNode(edit, hash, key, value));
    }
    // Same hash - update or add to collision list
    for (let i = 0; i < this.entries.length; i++) {
      if (Equal_.equals(this.entries[i][0], key)) {
        if (Equal_.equals(this.entries[i][1], value)) {
          return this;
        }
        if (this.canEdit(edit)) {
          this.entries[i] = [key, value];
          return this;
        }
        const newEntries = [...this.entries];
        newEntries[i] = [key, value];
        return new CollisionNode(edit, this.hash, newEntries);
      }
    }
    added.value = true;
    if (this.canEdit(edit)) {
      this.entries.push([key, value]);
      return this;
    }
    return new CollisionNode(edit, this.hash, [...this.entries, [key, value]]);
  }
  remove(edit, _shift, hash, key, removed) {
    if (this.canEdit(edit)) this._hash = undefined;
    if (this.hash !== hash) {
      return this;
    }
    const idx = this.entries.findIndex(([k]) => Equal_.equals(k, key));
    if (idx === -1) {
      return this;
    }
    removed.value = true;
    if (this.entries.length === 1) {
      return undefined;
    }
    if (this.entries.length === 2) {
      const remaining = this.entries[idx === 0 ? 1 : 0];
      return new LeafNode(edit, this.hash, remaining[0], remaining[1]);
    }
    if (this.canEdit(edit)) {
      this.entries.splice(idx, 1);
      return this;
    }
    const newEntries = [...this.entries];
    newEntries.splice(idx, 1);
    return new CollisionNode(edit, this.hash, newEntries);
  }
  computeHash() {
    let h = 0;
    const entries = this.entries;
    for (let i = 0; i < entries.length; i++) {
      h ^= entryHash(entries[i][0], entries[i][1]);
    }
    return h;
  }
}
/** @internal */
class IndexedNode extends Node {
  _tag = "IndexedNode";
  edit;
  _size;
  bitmap;
  children;
  constructor(edit, bitmap, children) {
    super();
    this.edit = edit;
    this.bitmap = bitmap;
    this.children = children;
  }
  get size() {
    if (this._size === undefined) {
      this._size = this.children.reduce((acc, child) => acc + child.size, 0);
    }
    return this._size;
  }
  get(shift, hash, key) {
    const bit = bitpos(hash, shift);
    if ((this.bitmap & bit) === 0) {
      return Option.none();
    }
    const idx = index(this.bitmap, bit);
    return this.children[idx].get(shift + SHIFT, hash, key);
  }
  has(shift, hash, key) {
    const bit = bitpos(hash, shift);
    if ((this.bitmap & bit) === 0) {
      return false;
    }
    const idx = index(this.bitmap, bit);
    return this.children[idx].has(shift + SHIFT, hash, key);
  }
  set(edit, shift, hash, key, value, added) {
    if (this.canEdit(edit)) this._hash = undefined;
    const bit = bitpos(hash, shift);
    const idx = index(this.bitmap, bit);
    if ((this.bitmap & bit) !== 0) {
      // Existing child - update it
      const child = this.children[idx];
      const newChild = child.set(edit, shift + SHIFT, hash, key, value, added);
      if (child === newChild) {
        return this;
      }
      if (this.canEdit(edit)) {
        this.children[idx] = newChild;
        return this;
      }
      const newChildren = [...this.children];
      newChildren[idx] = newChild;
      return new IndexedNode(edit, this.bitmap, newChildren);
    } else {
      // New child - insert
      added.value = true;
      const newChild = new LeafNode(edit, hash, key, value);
      const newBitmap = this.bitmap | bit;
      if (this.canEdit(edit)) {
        this.children.splice(idx, 0, newChild);
        this.bitmap = newBitmap;
        this._size = undefined;
        if (this.children.length > MAX_INDEX_NODE) {
          return this.expand(edit, newBitmap, this.children);
        }
        return this;
      }
      const newChildren = [...this.children];
      newChildren.splice(idx, 0, newChild);
      if (newChildren.length > MAX_INDEX_NODE) {
        return this.expand(edit, newBitmap, newChildren);
      }
      return new IndexedNode(edit, newBitmap, newChildren);
    }
  }
  remove(edit, shift, hash, key, removed) {
    if (this.canEdit(edit)) this._hash = undefined;
    const bit = bitpos(hash, shift);
    if ((this.bitmap & bit) === 0) {
      return this;
    }
    const idx = index(this.bitmap, bit);
    const child = this.children[idx];
    const newChild = child.remove(edit, shift + SHIFT, hash, key, removed);
    if (!removed.value) {
      return this;
    }
    if (newChild === undefined) {
      const newBitmap = this.bitmap ^ bit;
      if (newBitmap === 0) {
        return undefined;
      }
      if (this.children.length === 2) {
        const remaining = this.children[idx === 0 ? 1 : 0];
        if (remaining._tag === "LeafNode") {
          return remaining;
        }
      }
      if (this.canEdit(edit)) {
        this.children.splice(idx, 1);
        this.bitmap = newBitmap;
        this._size = undefined;
        return this;
      }
      const newChildren = [...this.children];
      newChildren.splice(idx, 1);
      return new IndexedNode(edit, newBitmap, newChildren);
    }
    if (child === newChild) {
      return this;
    }
    if (this.canEdit(edit)) {
      this.children[idx] = newChild;
      return this;
    }
    const newChildren = [...this.children];
    newChildren[idx] = newChild;
    return new IndexedNode(edit, this.bitmap, newChildren);
  }
  expand(edit, bitmap, children) {
    const nodes = new globalThis.Array(BUCKET_SIZE);
    let j = 0;
    for (let i = 0; i < BUCKET_SIZE; i++) {
      if ((bitmap & 1 << i) !== 0) {
        nodes[i] = children[j++];
      }
    }
    return new ArrayNode(edit, children.length, nodes);
  }
  computeHash() {
    return childrenHash(this.children);
  }
}
/** @internal */
class ArrayNode extends Node {
  _tag = "ArrayNode";
  edit;
  _size;
  count;
  children;
  constructor(edit, count, children) {
    super();
    this.edit = edit;
    this.count = count;
    this.children = children;
  }
  get size() {
    if (this._size === undefined) {
      this._size = this.children.reduce((acc, child) => acc + (child?.size ?? 0), 0);
    }
    return this._size;
  }
  get(shift, hash, key) {
    const idx = mask(hash, shift);
    const child = this.children[idx];
    return child ? child.get(shift + SHIFT, hash, key) : Option.none();
  }
  has(shift, hash, key) {
    const idx = mask(hash, shift);
    const child = this.children[idx];
    return child ? child.has(shift + SHIFT, hash, key) : false;
  }
  set(edit, shift, hash, key, value, added) {
    if (this.canEdit(edit)) this._hash = undefined;
    const idx = mask(hash, shift);
    const child = this.children[idx];
    if (child) {
      const newChild = child.set(edit, shift + SHIFT, hash, key, value, added);
      if (child === newChild) {
        return this;
      }
      if (this.canEdit(edit)) {
        this.children[idx] = newChild;
        return this;
      }
      const newChildren = [...this.children];
      newChildren[idx] = newChild;
      return new ArrayNode(edit, this.count, newChildren);
    } else {
      added.value = true;
      const newChild = new LeafNode(edit, hash, key, value);
      if (this.canEdit(edit)) {
        this.children[idx] = newChild;
        this.count++;
        this._size = undefined;
        return this;
      }
      const newChildren = [...this.children];
      newChildren[idx] = newChild;
      return new ArrayNode(edit, this.count + 1, newChildren);
    }
  }
  remove(edit, shift, hash, key, removed) {
    if (this.canEdit(edit)) this._hash = undefined;
    const idx = mask(hash, shift);
    const child = this.children[idx];
    if (!child) {
      return this;
    }
    const newChild = child.remove(edit, shift + SHIFT, hash, key, removed);
    if (!removed.value) {
      return this;
    }
    const newCount = this.count - (newChild ? 0 : 1);
    if (newCount < MIN_ARRAY_NODE) {
      return this.pack(edit, idx, newChild);
    }
    if (child === newChild) {
      return this;
    }
    if (this.canEdit(edit)) {
      this.children[idx] = newChild;
      if (!newChild) {
        this.count = newCount;
      }
      this._size = undefined;
      return this;
    }
    const newChildren = [...this.children];
    newChildren[idx] = newChild;
    return new ArrayNode(edit, newCount, newChildren);
  }
  pack(edit, excludeIdx, newChild) {
    const children = [];
    let bitmap = 0;
    let bit = 1;
    for (let i = 0; i < this.children.length; i++) {
      const child = i === excludeIdx ? newChild : this.children[i];
      if (child) {
        children.push(child);
        bitmap |= bit;
      }
      bit <<= 1;
    }
    return new IndexedNode(edit, bitmap, children);
  }
  computeHash() {
    return childrenHash(this.children);
  }
}
class HashMapIterator {
  stack;
  project;
  entries = [];
  index = 0;
  constructor(root, project) {
    this.stack = [root];
    this.project = project;
  }
  next() {
    while (true) {
      if (this.index < this.entries.length) {
        const entry = this.entries[this.index++];
        return {
          done: false,
          value: this.project(entry[0], entry[1])
        };
      }
      if (this.stack.length === 0) {
        return {
          done: true,
          value: undefined
        };
      }
      const node = this.stack.pop();
      switch (node._tag) {
        case "LeafNode":
          {
            const leaf = node;
            return {
              done: false,
              value: this.project(leaf.key, leaf.value)
            };
          }
        case "CollisionNode":
          {
            this.entries = node.entries;
            this.index = 0;
            break;
          }
        case "IndexedNode":
        case "ArrayNode":
          {
            const children = node.children;
            for (let i = children.length - 1; i >= 0; i--) {
              const child = children[i];
              if (child) this.stack.push(child);
            }
          }
      }
    }
  }
  [Symbol.iterator]() {
    return this;
  }
}
const toEntry = (key, value) => [key, value];
const toValue = (_, value) => value;
/** @internal */
class HashMapImpl {
  [HashMapTypeId] = HashMapTypeId;
  _editable;
  _edit;
  _root;
  _size;
  constructor(editable, edit, root, size) {
    this._editable = editable;
    this._edit = edit;
    this._root = root;
    this._size = size;
  }
  get size() {
    return this._size;
  }
  [Symbol.iterator]() {
    return new HashMapIterator(this._root, toEntry);
  }
  [Equal_.symbol](that) {
    if (isHashMap(that)) {
      const thatImpl = that;
      if (this.size !== thatImpl.size) {
        return false;
      }
      for (const [key, value] of this) {
        const otherValue = pipe(that, get(key));
        if (Option.isNone(otherValue) || !Equal_.equals(value, otherValue.value)) {
          return false;
        }
      }
      return true;
    }
    return false;
  }
  [Hash.symbol]() {
    return Hash.optimize(hashMapSeed ^ this._root.subtreeHash());
  }
  [NodeInspectSymbol]() {
    return toJson(this);
  }
  toString() {
    return `HashMap(${format(Array.from(this))})`;
  }
  toJSON() {
    return {
      _id: "HashMap",
      values: Array.from(this).map(([k, v]) => [toJson(k), toJson(v)])
    };
  }
  pipe() {
    return pipeArguments(this, arguments);
  }
}
/** @internal */
const emptyNode = /*#__PURE__*/new EmptyNode();
/** @internal */
export const isHashMap = u => hasProperty(u, HashMapTypeId);
/** @internal */
export const empty = () => new HashMapImpl(false, 0, emptyNode, 0);
/** @internal */
export const make = (...entries) => fromIterable(entries);
/** @internal */
export const fromIterable = entries => {
  let root = emptyNode;
  let size = 0;
  const added = {
    value: false
  };
  for (const [key, value] of entries) {
    const hash = Hash.hash(key);
    added.value = false;
    root = root.set(NaN, 0, hash, key, value, added);
    if (added.value) {
      size++;
    }
  }
  return new HashMapImpl(false, 0, root, size);
};
/** @internal */
export const isEmpty = self => self.size === 0;
/** @internal */
export const get = /*#__PURE__*/dual(2, (self, key) => {
  const impl = self;
  return impl._root.get(0, Hash.hash(key), key);
});
/** @internal */
export const getHash = /*#__PURE__*/dual(3, (self, key, hash) => {
  const impl = self;
  return impl._root.get(0, hash, key);
});
/** @internal */
export const getUnsafe = /*#__PURE__*/dual(2, (self, key) => {
  const result = get(self, key);
  if (Option.isSome(result)) {
    return result.value;
  }
  throw new Error(`HashMap.getUnsafe: key not found: ${key}`);
});
/** @internal */
export const has = /*#__PURE__*/dual(2, (self, key) => {
  const impl = self;
  return impl._root.has(0, Hash.hash(key), key);
});
/** @internal */
export const hasHash = /*#__PURE__*/dual(3, (self, key, hash) => {
  const impl = self;
  return impl._root.has(0, hash, key);
});
/** @internal */
export const hasBy = /*#__PURE__*/dual(2, (self, predicate) => {
  for (const [key, value] of self) {
    if (predicate(value, key)) {
      return true;
    }
  }
  return false;
});
const setHash = (self, key, hash, value) => {
  const impl = self;
  const added = {
    value: false
  };
  // Pass edit context: use current edit if editable, otherwise NaN (never matches any edit)
  const edit = impl._editable ? impl._edit : NaN;
  const newRoot = impl._root.set(edit, 0, hash, key, value, added);
  if (impl._editable) {
    // In-place mutation
    impl._root = newRoot;
    if (added.value) {
      impl._size++;
    }
    return self;
  }
  // Immutable: create new instance if changed
  if (impl._root === newRoot) {
    return self;
  }
  return new HashMapImpl(false, impl._edit, newRoot, impl._size + (added.value ? 1 : 0));
};
/** @internal */
export const set = /*#__PURE__*/dual(3, (self, key, value) => {
  return setHash(self, key, Hash.hash(key), value);
});
/** @internal */
export const keys = self => new HashMapIterator(self._root, identity);
/** @internal */
export const values = self => new HashMapIterator(self._root, toValue);
/** @internal */
export const entries = self => self[Symbol.iterator]();
/** @internal */
export const size = self => self.size;
/** @internal */
export const beginMutation = self => {
  const impl = self;
  return new HashMapImpl(true, impl._edit + 1, impl._root, impl._size);
};
/** @internal */
export const endMutation = self => {
  const impl = self;
  impl._editable = false;
  return self;
};
/** @internal */
export const mutate = /*#__PURE__*/dual(2, (self, f) => {
  const mutable = beginMutation(self);
  f(mutable);
  return endMutation(mutable);
});
/** @internal */
export const modifyAt = /*#__PURE__*/dual(3, (self, key, f) => {
  const current = get(self, key);
  const updated = f(current);
  if (Option.isNone(updated)) {
    return has(self, key) ? remove(self, key) : self;
  }
  return set(self, key, updated.value);
});
/** @internal */
export const modifyHash = /*#__PURE__*/dual(4, (self, key, hash, f) => {
  const current = getHash(self, key, hash);
  const updated = f(current);
  if (Option.isNone(updated)) {
    return hasHash(self, key, hash) ? removeHash(self, key, hash) : self;
  }
  return setHash(self, key, hash, updated.value);
});
/** @internal */
export const modify = /*#__PURE__*/dual(3, (self, key, f) => {
  return modifyAt(self, key, Option.map(f));
});
/** @internal */
export const union = /*#__PURE__*/dual(2, (self, that) => {
  let result = self;
  for (const [key, value] of that) {
    result = set(result, key, value);
  }
  return result;
});
const removeHash = (self, key, hash) => {
  const impl = self;
  const removed = {
    value: false
  };
  const edit = impl._editable ? impl._edit : NaN;
  const newRoot = impl._root.remove(edit, 0, hash, key, removed);
  if (!removed.value) {
    return self;
  }
  if (impl._editable) {
    impl._root = newRoot ?? emptyNode;
    impl._size--;
    return self;
  }
  if (newRoot === undefined) {
    return empty();
  }
  return new HashMapImpl(false, impl._edit, newRoot, impl._size - 1);
};
/** @internal */
export const remove = /*#__PURE__*/dual(2, (self, key) => {
  return removeHash(self, key, Hash.hash(key));
});
/** @internal */
export const removeMany = /*#__PURE__*/dual(2, (self, keys) => {
  let result = self;
  for (const key of keys) {
    result = remove(result, key);
  }
  return result;
});
/** @internal */
export const setMany = /*#__PURE__*/dual(2, (self, entries) => {
  let result = self;
  for (const [key, value] of entries) {
    result = set(result, key, value);
  }
  return result;
});
/** @internal */
export const map = /*#__PURE__*/dual(2, (self, f) => {
  let result = empty();
  for (const [key, value] of self) {
    result = set(result, key, f(value, key));
  }
  return result;
});
/** @internal */
export const flatMap = /*#__PURE__*/dual(2, (self, f) => {
  let result = empty();
  for (const [key, value] of self) {
    result = union(result, f(value, key));
  }
  return result;
});
/** @internal */
export const forEach = /*#__PURE__*/dual(2, (self, f) => {
  for (const [key, value] of self) {
    f(value, key);
  }
});
/** @internal */
export const reduce = /*#__PURE__*/dual(3, (self, zero, f) => {
  let result = zero;
  for (const [key, value] of self) {
    result = f(result, value, key);
  }
  return result;
});
/** @internal */
export const filter = /*#__PURE__*/dual(2, (self, f) => {
  let result = empty();
  for (const [key, value] of self) {
    if (f(value, key)) {
      result = set(result, key, value);
    }
  }
  return result;
});
/** @internal */
export const compact = self => {
  let result = empty();
  for (const [key, value] of self) {
    if (Option.isSome(value)) {
      result = set(result, key, value.value);
    }
  }
  return result;
};
/** @internal */
export const filterMap = /*#__PURE__*/dual(2, (self, f) => {
  let result = empty();
  for (const [key, value] of self) {
    const mapped = f(value, key);
    if (Result.isSuccess(mapped)) {
      result = set(result, key, mapped.success);
    }
  }
  return result;
});
/** @internal */
export const findFirst = /*#__PURE__*/dual(2, (self, predicate) => {
  for (const [key, value] of self) {
    if (predicate(value, key)) {
      return Option.some([key, value]);
    }
  }
  return Option.none();
});
/** @internal */
export const some = /*#__PURE__*/dual(2, (self, predicate) => {
  for (const [key, value] of self) {
    if (predicate(value, key)) {
      return true;
    }
  }
  return false;
});
/** @internal */
export const every = /*#__PURE__*/dual(2, (self, predicate) => {
  for (const [key, value] of self) {
    if (!predicate(value, key)) {
      return false;
    }
  }
  return true;
});
//# sourceMappingURL=hashMap.js.map