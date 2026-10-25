import * as Data from "../Data.js";
import * as Equal from "../Equal.js";
import { dual } from "../Function.js";
import * as Hash from "../Hash.js";
import { NodeInspectSymbol } from "../Inspectable.js";
import * as Option from "../Option.js";
import { hasProperty } from "../Predicate.js";
import * as Result from "../Result.js";
const TypeId = "~effect/net/NetAddress";
const MulticastTypeId = "~effect/net/NetAddress/MulticastAddress";
const UnicastTypeId = "~effect/net/NetAddress/UnicastAddress";
const BroadcastTypeId = "~effect/net/NetAddress/BroadcastAddress";
const LoopbackTypeId = "~effect/net/NetAddress/LoopbackAddress";
const LinkLocalTypeId = "~effect/net/NetAddress/LinkLocalAddress";
const UnspecifiedTypeId = "~effect/net/NetAddress/UnspecifiedAddress";
const PrivateTypeId = "~effect/net/NetAddress/PrivateAddress";
const UniqueLocalTypeId = "~effect/net/NetAddress/UniqueLocalAddress";
const LocallyAdministeredTypeId = "~effect/net/NetAddress/LocallyAdministeredAddress";
const UniversallyAdministeredTypeId = "~effect/net/NetAddress/UniversallyAdministeredAddress";
// IPv4 addresses store one unsigned 32-bit number and IPv6 addresses store four
// unsigned 32-bit words, most significant first. Allocating a typed array per
// address is expensive on some runtimes, notably Deno.
const ipv4Value = self => self.value;
const ipv6Words = self => self;
const getMacBytes = self => self.bytes;
/**
 * A checked network-address operation failure retaining the address or supplied input.
 *
 * **Details**
 *
 * Address-based operations retain the address value. Parsing and numeric
 * construction retain the supplied string or array. Composed operations forward
 * errors from the failing operation unchanged. Failures from external operations
 * retain the original exception in `cause` when available.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class NetAddressError extends /*#__PURE__*/Data.TaggedError("NetAddressError") {}
const isAddress = u => hasProperty(u, TypeId);
/**
 * Returns `true` when a value is an IPv4 address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isIpv4Address = u => isAddress(u) && u._tag === "Ipv4Address";
/**
 * Returns `true` when a value is an IPv6 address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isIpv6Address = u => isAddress(u) && u._tag === "Ipv6Address";
/**
 * Returns `true` when a value is an IPv4 or IPv6 address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isIpAddress = u => isIpv4Address(u) || isIpv6Address(u);
/**
 * Returns the bit width of an IP address.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const width = address => isIpv4Address(address) ? 32 : 128;
/**
 * Returns `true` when a value is a MAC address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isMacAddress = u => isAddress(u) && u._tag === "MacAddress";
/**
 * Returns `true` when a value is a resolved IPv4 internet address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isInetAddressV4 = u => isAddress(u) && u._tag === "InetAddressV4";
/**
 * Returns `true` when a value is a resolved IPv6 internet address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isInetAddressV6 = u => isAddress(u) && u._tag === "InetAddressV6";
/**
 * Returns `true` when a value is a resolved internet address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isInetAddress = u => isInetAddressV4(u) || isInetAddressV6(u);
/**
 * Returns `true` when a value is a Unix-domain filesystem address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isUnixPathAddress = u => isAddress(u) && u._tag === "UnixPathAddress";
/**
 * Returns `true` when a value is a portable concrete socket address.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isSocketAddress = u => isInetAddress(u) || isUnixPathAddress(u);
const Ipv4Proto = {
  _tag: "Ipv4Address",
  [TypeId]: TypeId,
  [Equal.symbol](that) {
    return isIpv4Address(that) && ipv4Value(this) === ipv4Value(that);
  },
  [Hash.symbol]() {
    return Hash.optimize(Hash.combine(ipv4HashSeed, ipv4Value(this) | 0));
  },
  toString() {
    return formatIp(this);
  },
  toJSON() {
    return this.toString();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
const Ipv6Proto = {
  _tag: "Ipv6Address",
  [TypeId]: TypeId,
  [Equal.symbol](that) {
    if (!isIpv6Address(that)) return false;
    const self = ipv6Words(this);
    const other = ipv6Words(that);
    return self.w0 === other.w0 && self.w1 === other.w1 && self.w2 === other.w2 && self.w3 === other.w3;
  },
  [Hash.symbol]() {
    const self = ipv6Words(this);
    let h = Hash.combine(ipv6HashSeed, self.w0 | 0);
    h = Hash.combine(h, self.w1 | 0);
    h = Hash.combine(h, self.w2 | 0);
    return Hash.optimize(Hash.combine(h, self.w3 | 0));
  },
  toString() {
    return formatIp(this);
  },
  toJSON() {
    return this.toString();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
const MacProto = {
  _tag: "MacAddress",
  [TypeId]: TypeId,
  [Equal.symbol](that) {
    return isMacAddress(that) && bytesEqual(getMacBytes(this), getMacBytes(that));
  },
  [Hash.symbol]() {
    return Hash.combine(macHashSeed, Hash.array(getMacBytes(this)));
  },
  toString() {
    return formatMacAddress(this);
  },
  toJSON() {
    return this.toString();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
const bytesEqual = (self, that) => {
  if (self.length !== that.length) return false;
  for (let index = 0; index < self.length; index++) {
    if (self[index] !== that[index]) return false;
  }
  return true;
};
const ipv4HashSeed = /*#__PURE__*/Hash.string("Ipv4Address");
const ipv6HashSeed = /*#__PURE__*/Hash.string("Ipv6Address");
const macHashSeed = /*#__PURE__*/Hash.string("MacAddress");
const makeIpv4 = value => {
  const self = Object.create(Ipv4Proto);
  self.value = value >>> 0;
  return Object.freeze(self);
};
const makeIpv6 = (w0, w1, w2, w3) => {
  const self = Object.create(Ipv6Proto);
  self.w0 = w0 >>> 0;
  self.w1 = w1 >>> 0;
  self.w2 = w2 >>> 0;
  self.w3 = w3 >>> 0;
  return Object.freeze(self);
};
const packOctets = (a, b, c, d) => (a << 24 | b << 16 | c << 8 | d) >>> 0;
const packBytes = (bytes, offset) => packOctets(bytes[offset], bytes[offset + 1], bytes[offset + 2], bytes[offset + 3]);
const wordToOctets = word => [word >>> 24, word >>> 16 & 0xff, word >>> 8 & 0xff, word & 0xff];
const makeMac = bytes => {
  const self = Object.assign(Object.create(MacProto), {
    bytes: new Uint8Array(bytes)
  });
  return Object.freeze(self);
};
/**
 * Creates an IPv4 address from trusted network-order bytes without validation.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const ipv4FromBytesUnsafe = bytes => makeIpv4(packBytes(bytes, 0));
/**
 * Creates an IPv6 address from trusted network-order bytes without validation.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const ipv6FromBytesUnsafe = bytes => makeIpv6(packBytes(bytes, 0), packBytes(bytes, 4), packBytes(bytes, 8), packBytes(bytes, 12));
/**
 * The IPv4 loopback address `127.0.0.1`.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const ipv4Loopback = /*#__PURE__*/makeIpv4(0x7f000001);
/**
 * The IPv6 loopback address `::1`.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const ipv6Loopback = /*#__PURE__*/makeIpv6(0, 0, 0, 1);
/**
 * The unspecified IPv4 address `0.0.0.0`.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const ipv4Unspecified = /*#__PURE__*/makeIpv4(0);
/**
 * The unspecified IPv6 address `::`.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const ipv6Unspecified = /*#__PURE__*/makeIpv6(0, 0, 0, 0);
/**
 * The IPv4 broadcast address `255.255.255.255`.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const ipv4Broadcast = /*#__PURE__*/makeIpv4(0xffffffff);
const addressError = (input, message) => Result.fail(new NetAddressError({
  input,
  message
}));
/**
 * Creates an IPv4 address from four checked octets.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const ipv4FromOctets = octets => {
  if (!octets.every(n => Number.isInteger(n) && n >= 0 && n <= 255)) {
    return addressError(octets, "octets must be integers from 0 through 255");
  }
  return Result.succeed(makeIpv4(packOctets(octets[0], octets[1], octets[2], octets[3])));
};
/**
 * Creates an IPv6 address from eight checked 16-bit segments.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const ipv6FromSegments = segments => {
  if (!segments.every(n => Number.isInteger(n) && n >= 0 && n <= 0xffff)) {
    return addressError(segments, "segments must be integers from 0 through 65535");
  }
  return Result.succeed(makeIpv6(segments[0] * 0x10000 + segments[1], segments[2] * 0x10000 + segments[3], segments[4] * 0x10000 + segments[5], segments[6] * 0x10000 + segments[7]));
};
/**
 * Creates a MAC address from six checked octets.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const macAddressFromOctets = octets => {
  if (!octets.every(n => Number.isInteger(n) && n >= 0 && n <= 255)) {
    return addressError(octets, "octets must be integers from 0 through 255");
  }
  return Result.succeed(makeMac(new Uint8Array(octets)));
};
/**
 * Parses a colon-separated MAC address containing six two-digit hexadecimal octets.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const macAddressFromString = input => {
  if (!/^(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$/.test(input)) {
    return addressError(input, "expected six two-digit hexadecimal octets separated by colons");
  }
  return Result.succeed(makeMac(new Uint8Array(input.split(":").map(part => Number.parseInt(part, 16)))));
};
/**
 * Parses a trusted colon-separated MAC address, throwing on failure.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const macAddressFromStringUnsafe = input => Result.getOrThrow(macAddressFromString(input));
/**
 * Parses a strict dotted-decimal IPv4 address.
 *
 * **Details**
 *
 * Multi-digit octets with a leading zero are rejected to avoid octal ambiguity.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const ipv4FromString = input => {
  const parts = input.split(".");
  if (parts.length !== 4 || parts.some(part => !/^\d{1,3}$/.test(part))) {
    return addressError(input, "expected exactly four decimal octets");
  }
  if (parts.some(part => part.length > 1 && part[0] === "0")) {
    return addressError(input, "leading zeroes are not allowed");
  }
  const octets = parts.map(Number);
  if (octets.some(part => part > 255)) {
    return addressError(input, "octets must be at most 255");
  }
  return ipv4FromOctets([octets[0], octets[1], octets[2], octets[3]]);
};
const parseIpv6Segments = input => {
  if (input.includes("[") || input.includes("]") || input.includes("%")) {
    return addressError(input, "brackets and zone identifiers are not valid in a bare IPv6 address");
  }
  const halves = input.split("::");
  if (halves.length > 2) return addressError(input, "only one compression marker is allowed");
  const head = halves[0] === "" ? [] : halves[0].split(":");
  const tail = halves.length === 2 && halves[1] !== "" ? halves[1].split(":") : [];
  if (head.some(part => part === "") || tail.some(part => part === "")) {
    return addressError(input, "empty segments are only valid in the compression marker");
  }
  const trailing = tail.length > 0 ? tail[tail.length - 1] : head.length > 0 ? head[head.length - 1] : "";
  let embedded;
  if (trailing.includes(".")) {
    if (halves.length === 2 && tail.length === 0) {
      return addressError(input, "embedded IPv4 syntax must be trailing");
    }
    const parsed = Result.map(ipv4FromString(trailing), address => {
      const value = ipv4Value(address);
      return [value >>> 16, value & 0xffff];
    });
    if (Result.isFailure(parsed)) return parsed;
    embedded = parsed.success;
    if (tail.length > 0) tail.pop();else head.pop();
  }
  const explicit = head.length + tail.length + (embedded ? 2 : 0);
  if (halves.length === 1 ? explicit !== 8 : explicit >= 8) {
    return addressError(input, halves.length === 1 ? "expected eight segments" : "compression must replace at least one segment");
  }
  const parse = part => /^[0-9a-fA-F]{1,4}$/.test(part) ? Number.parseInt(part, 16) : undefined;
  const parsedHead = head.map(parse);
  const parsedTail = tail.map(parse);
  if (parsedHead.some(part => part === undefined) || parsedTail.some(part => part === undefined)) {
    return addressError(input, "segments must contain one through four hexadecimal digits");
  }
  return Result.succeed([...parsedHead, ...Array(8 - explicit).fill(0), ...parsedTail, ...(embedded ?? [])]);
};
/**
 * Parses an IPv6 address with optional compression and trailing embedded IPv4.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const ipv6FromString = input => {
  return Result.flatMap(parseIpv6Segments(input), segments => ipv6FromSegments(segments));
};
/**
 * Parses a bare numeric IPv4 or IPv6 address.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const ipFromString = input => {
  const result = input.includes(":") ? ipv6FromString(input) : ipv4FromString(input);
  return result;
};
/**
 * Parses a trusted bare numeric IPv4 or IPv6 address, throwing on failure.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const ipFromStringUnsafe = input => Result.getOrThrow(ipFromString(input));
/**
 * Returns the four numeric octets of an IPv4 address in a fresh tuple.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const ipv4ToOctets = self => wordToOctets(ipv4Value(self));
/**
 * Returns the eight numeric segments of an IPv6 address in a fresh tuple.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const ipv6ToSegments = self => {
  const {
    w0,
    w1,
    w2,
    w3
  } = ipv6Words(self);
  return [w0 >>> 16, w0 & 0xffff, w1 >>> 16, w1 & 0xffff, w2 >>> 16, w2 & 0xffff, w3 >>> 16, w3 & 0xffff];
};
/**
 * Returns the sixteen numeric octets of an IPv6 address in a fresh array.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const ipv6ToOctets = self => {
  const {
    w0,
    w1,
    w2,
    w3
  } = ipv6Words(self);
  return [...wordToOctets(w0), ...wordToOctets(w1), ...wordToOctets(w2), ...wordToOctets(w3)];
};
/**
 * Returns the six numeric octets of a MAC address in a fresh tuple.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const macAddressToOctets = self => {
  const bytes = getMacBytes(self);
  return [bytes[0], bytes[1], bytes[2], bytes[3], bytes[4], bytes[5]];
};
/**
 * Formats a MAC address as six lowercase hexadecimal octets separated by colons.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatMacAddress = self => Array.from(getMacBytes(self), byte => byte.toString(16).padStart(2, "0")).join(":");
/**
 * Returns `true` when the MAC address is the all-ones broadcast address.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isMacBroadcast = self => getMacBytes(self).every(byte => byte === 0xff);
/**
 * Returns `true` when the MAC address has the IEEE group-address bit set.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isMacMulticast = self => (getMacBytes(self)[0] & 1) !== 0;
/**
 * Returns `true` when the MAC address has the IEEE group-address bit clear.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isMacUnicast = self => (getMacBytes(self)[0] & 1) === 0;
/**
 * Returns `true` when the MAC address has the IEEE local-administration bit set.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isMacLocallyAdministered = self => (getMacBytes(self)[0] & 2) !== 0;
/**
 * Returns `true` when the MAC address has the IEEE local-administration bit clear.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isMacUniversallyAdministered = self => (getMacBytes(self)[0] & 2) === 0;
/**
 * Folds an IP address by its numeric version.
 *
 * @stability unstable
 * @category pattern matching
 * @since 4.0.0
 */
export const match = /*#__PURE__*/dual(2, (self, options) => isIpv4Address(self) ? options.onIpv4(self) : options.onIpv6(self));
/**
 * Formats an IP address in canonical numeric form.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatIp = self => {
  if (isIpv4Address(self)) return ipv4ToOctets(self).join(".");
  const segments = ipv6ToSegments(self);
  if (isIpv4Mapped(self)) {
    return `::ffff:${segments[6] >> 8}.${segments[6] & 0xff}.${segments[7] >> 8}.${segments[7] & 0xff}`;
  }
  let bestStart = -1;
  let bestLength = 0;
  let start = -1;
  for (let index = 0; index <= 8; index++) {
    if (index < 8 && segments[index] === 0) {
      if (start === -1) start = index;
    } else if (start !== -1) {
      if (index - start > bestLength) {
        bestStart = start;
        bestLength = index - start;
      }
      start = -1;
    }
  }
  if (bestLength < 2) return segments.map(segment => segment.toString(16)).join(":");
  const head = segments.slice(0, bestStart).map(segment => segment.toString(16)).join(":");
  const tail = segments.slice(bestStart + bestLength).map(segment => segment.toString(16)).join(":");
  return `${head}::${tail}`;
};
/**
 * Returns `true` for the all-zero address of either IP version.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isUnspecified = self => {
  if (isIpv4Address(self)) return ipv4Value(self) === 0;
  const {
    w0,
    w1,
    w2,
    w3
  } = ipv6Words(self);
  return w0 === 0 && w1 === 0 && w2 === 0 && w3 === 0;
};
/**
 * Returns `true` for IPv4 `127.0.0.0/8` or IPv6 `::1`.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isLoopback = self => {
  if (isIpv4Address(self)) return ipv4Value(self) >>> 24 === 127;
  const {
    w0,
    w1,
    w2,
    w3
  } = ipv6Words(self);
  return w0 === 0 && w1 === 0 && w2 === 0 && w3 === 1;
};
/**
 * Returns `true` for IPv4 `224.0.0.0/4`, IPv6 `ff00::/8`, or a MAC address with
 * the IEEE group bit set, refining the value while preserving its address type.
 *
 * **Details**
 *
 * The MAC all-ones broadcast address has the group bit set and is multicast;
 * the IPv4 limited broadcast address `255.255.255.255` is not. Use
 * {@link isMacBroadcast} or {@link isBroadcast} to distinguish broadcast.
 *
 * **Example** (Classifying IP and MAC group addresses)
 *
 * ```ts import.meta.vitest
 * import { assert } from "@effect/vitest"
 * import { NetAddress } from "effect/net"
 *
 * const ip = NetAddress.ipFromStringUnsafe("239.255.0.1")
 * const mac = NetAddress.macAddressFromStringUnsafe("ff:ff:ff:ff:ff:ff")
 *
 * assert.isTrue(NetAddress.isMulticast(ip))
 * assert.isTrue(NetAddress.isMulticast(mac))
 * assert.isTrue(NetAddress.isBroadcast(mac))
 * assert.isFalse(NetAddress.isMulticast(NetAddress.ipv4Broadcast))
 * ```
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isMulticast = self => {
  if (isMacAddress(self)) return isMacMulticast(self);
  if (isIpv4Address(self)) return ipv4Value(self) >>> 28 === 0xe;
  return ipv6Words(self).w0 >>> 24 === 0xff;
};
/**
 * Returns `true` for the IPv4 limited broadcast address `255.255.255.255` or
 * the MAC all-ones broadcast address.
 *
 * **Details**
 *
 * MAC broadcast is also multicast because its IEEE group bit is set. IPv4
 * limited broadcast is not multicast. Directed IPv4 broadcast cannot be
 * determined without a network prefix and is not classified here.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isBroadcast = self => isMacAddress(self) ? isMacBroadcast(self) : ipv4Value(self) === 0xffffffff;
/**
 * Returns `true` when an address is syntactically unicast.
 *
 * **Details**
 *
 * IP addresses exclude multicast, unspecified, and IPv4 limited broadcast.
 * This does not exclude reserved ranges, prefix-relative directed broadcast,
 * or addresses that are unusable or unreachable in a particular deployment.
 * MAC addresses are unicast when their IEEE individual/group bit is clear.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isUnicast = self => {
  if (isMacAddress(self)) return isMacUnicast(self);
  return !isMulticast(self) && !isUnspecified(self) && (!isIpv4Address(self) || !isBroadcast(self));
};
/**
 * Returns `true` for IPv4 `169.254.0.0/16` or IPv6 `fe80::/10`.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isLinkLocal = self => {
  if (isIpv4Address(self)) return ipv4Value(self) >>> 16 === 0xa9fe;
  return (ipv6Words(self).w0 >>> 16 & 0xffc0) === 0xfe80;
};
/**
 * Returns `true` for IPv4 private-use ranges defined by RFC 1918.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isPrivate = self => {
  const value = ipv4Value(self);
  return value >>> 24 === 10 || (value >>> 16 & 0xfff0) === 0xac10 || value >>> 16 === 0xc0a8;
};
/**
 * Returns `true` for IPv6 unique-local addresses in `fc00::/7`.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isUniqueLocal = self => (ipv6Words(self).w0 >>> 24 & 0xfe) === 0xfc;
/**
 * Returns `true` when an IPv6 address is in the `::ffff:0:0/96` mapped range.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isIpv4Mapped = self => {
  const {
    w0,
    w1,
    w2
  } = ipv6Words(self);
  return w0 === 0 && w1 === 0 && w2 === 0xffff;
};
/**
 * Converts an IPv4 address to its IPv4-mapped IPv6 representation.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toIpv4Mapped = self => makeIpv6(0, 0, 0xffff, ipv4Value(self));
/**
 * Extracts the IPv4 value from an IPv4-mapped IPv6 address.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const fromIpv4Mapped = self => isIpv4Mapped(self) ? Option.some(makeIpv4(ipv6Words(self).w3)) : Option.none();
/**
 * @stability unstable
 */
export function toCanonical(self) {
  if (isInetAddress(self)) {
    const address = toCanonical(self.address);
    return address === self.address ? self : inetAddressUnsafe(address, self.port);
  }
  return isIpv6Address(self) ? Option.getOrElse(fromIpv4Mapped(self), () => self) : self;
}
const InetV4Proto = {
  _tag: "InetAddressV4",
  [TypeId]: TypeId,
  [Equal.symbol](that) {
    return isInetAddressV4(that) && this.port === that.port && Equal.equals(this.address, that.address);
  },
  [Hash.symbol]() {
    return Hash.combine(Hash.hash(this.address), Hash.number(this.port));
  },
  toString() {
    return formatInet(this);
  },
  toJSON() {
    return this.toString();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
const InetV6Proto = {
  _tag: "InetAddressV6",
  [TypeId]: TypeId,
  [Equal.symbol](that) {
    return isInetAddressV6(that) && this.port === that.port && this.scopeId === that.scopeId && Equal.equals(this.address, that.address);
  },
  [Hash.symbol]() {
    return Hash.combine(Hash.combine(Hash.hash(this.address), Hash.number(this.port)), Hash.number(this.scopeId));
  },
  toString() {
    return formatInet(this);
  },
  toJSON() {
    return this.toString();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
const checkPort = port => Number.isInteger(port) && port >= 0 && port <= 0xffff;
/**
 * Creates a checked IPv4 internet address.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const inetAddressV4 = (address, port) => {
  if (!checkPort(port)) {
    return addressError(address, "port must be an integer from 0 through 65535");
  }
  const self = Object.create(InetV4Proto);
  self.address = address;
  self.port = port;
  return Result.succeed(Object.freeze(self));
};
/**
 * Creates a checked IPv6 internet address with an optional scope identifier.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const inetAddressV6 = (address, port, options) => {
  if (!checkPort(port)) {
    return addressError(address, "port must be an integer from 0 through 65535");
  }
  const scopeId = options?.scopeId ?? 0;
  if (!Number.isInteger(scopeId) || scopeId < 0 || scopeId > 0xffffffff) {
    return addressError(address, "scopeId must be an unsigned 32-bit integer");
  }
  const self = Object.create(InetV6Proto);
  self.address = address;
  self.port = port;
  self.scopeId = scopeId;
  return Result.succeed(Object.freeze(self));
};
/**
 * Creates a checked internet address for an IP address and port.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const inetAddress = (address, port) => isIpv4Address(address) ? inetAddressV4(address, port) : inetAddressV6(address, port);
/**
 * Creates an internet address from a trusted IP address and port, throwing on failure.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const inetAddressUnsafe = (address, port) => Result.getOrThrow(inetAddress(address, port));
/**
 * Creates a checked internet address from a numeric IP string and port.
 *
 * **Gotchas**
 *
 * The address must be a numeric IPv4 or IPv6 literal. This function does not
 * resolve hostnames or accept IPv6 brackets.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const inetAddressFromIpString = (address, port) => {
  return Result.flatMap(ipFromString(address), address => inetAddress(address, port));
};
/**
 * Creates an internet address from a trusted numeric IP string and port.
 *
 * **Gotchas**
 *
 * This function throws when either input is invalid. Use only when both values
 * are already known to satisfy the checked constructor's requirements.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const inetAddressFromIpStringUnsafe = (address, port) => Result.getOrThrow(inetAddressFromIpString(address, port));
/**
 * Parses an unbracketed numeric host and port, resolving named IPv6 zones using
 * a supplied map of interface names to numeric scope IDs.
 *
 * **Details**
 *
 * IPv4, unscoped IPv6, and numeric IPv6 zones need no map. Named zones must have
 * a matching map entry. This function performs no DNS or operating-system lookup.
 *
 * @see {@link formatHost} for formatting the host of an internet address
 * @see {@link scopeIdsFromInterfaces} for building a scope map from interface entries
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const inetAddressFromHostString = (host, port, scopeIds) => {
  const separator = host.indexOf("%");
  if (separator !== -1) {
    const zone = host.slice(separator + 1);
    if (zone.length === 0 || zone.includes("%")) {
      return addressError(host, "invalid IPv6 scope identifier");
    }
    if (!/^\d+$/.test(zone)) {
      const scopeId = scopeIds?.get(zone);
      if (scopeId === undefined) {
        return addressError(host, `unknown IPv6 interface: ${zone}`);
      }
      host = `${host.slice(0, separator)}%${scopeId}`;
    }
  }
  return inetAddressFromString(host.includes(":") ? `[${host}]:${port}` : `${host}:${port}`);
};
/**
 * Creates a map from interface names to IPv6 scope IDs using supplied interface
 * entries.
 *
 * **Details**
 *
 * Each entry pairs an interface name with its addresses. The first IPv6 address
 * with a positive scope ID supplies that interface's mapping. Accepts entries
 * such as `Object.entries(os.networkInterfaces())` without performing any
 * operating-system lookup itself. Later changes to the entries do not affect
 * the map.
 *
 * @see {@link inetAddressFromHostString} for resolving named IPv6 zones with the map
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const scopeIdsFromInterfaces = interfaces => {
  const scopeIds = new Map();
  for (const [name, addresses] of interfaces) {
    const address = addresses?.find(address => address.family === "IPv6" && (address.scopeid ?? 0) > 0);
    if (address?.scopeid !== undefined) scopeIds.set(name, address.scopeid);
  }
  return scopeIds;
};
/**
 * Parses `IPv4:port` or `[IPv6]:port` without DNS resolution.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const inetAddressFromString = input => {
  let host;
  let portText;
  let scopeId = 0;
  const bracketed = input.startsWith("[");
  if (bracketed) {
    const end = input.indexOf("]");
    if (end < 0 || input[end + 1] !== ":" || input.indexOf("]", end + 1) !== -1) {
      return addressError(input, "expected [IPv6]:port");
    }
    host = input.slice(1, end);
    portText = input.slice(end + 2);
    const scopeSeparator = host.indexOf("%");
    if (scopeSeparator !== -1) {
      const scopeText = host.slice(scopeSeparator + 1);
      if (host.indexOf("%", scopeSeparator + 1) !== -1 || !/^\d+$/.test(scopeText)) {
        return addressError(input, "scope identifier must be decimal");
      }
      scopeId = Number(scopeText);
      if (!Number.isInteger(scopeId) || scopeId > 0xffffffff) {
        return addressError(input, "scope identifier must be an unsigned 32-bit integer");
      }
      host = host.slice(0, scopeSeparator);
    }
  } else {
    const separator = input.lastIndexOf(":");
    if (separator < 0) {
      return addressError(input, "expected host:port or [IPv6]:port");
    }
    if (input.indexOf(":") !== separator) {
      return addressError(input, "IPv6 addresses must be bracketed");
    }
    host = input.slice(0, separator);
    portText = input.slice(separator + 1);
  }
  if (!/^(0|[1-9]\d*)$/.test(portText)) {
    return addressError(input, "port must be an unpadded decimal integer");
  }
  return Result.flatMap(ipFromString(host), address => {
    if (bracketed !== isIpv6Address(address)) {
      return addressError(input, "only IPv6 addresses use brackets");
    }
    return isIpv6Address(address) ? inetAddressV6(address, Number(portText), {
      scopeId
    }) : inetAddressV4(address, Number(portText));
  });
};
/**
 * Parses a trusted numeric internet address and port, throwing on failure.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const inetAddressFromStringUnsafe = input => Result.getOrThrow(inetAddressFromString(input));
/**
 * Formats the numeric host of an internet address without brackets or a port,
 * preserving a nonzero IPv6 scope ID as a `%` suffix.
 *
 * **When to use**
 *
 * Use when the host and port are represented separately.
 *
 * @see {@link formatInet} for a complete socket address
 * @see {@link formatUrlHost} for a bracketed URL authority host
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatHost = self => formatIp(self.address) + (isInetAddressV6(self) && self.scopeId !== 0 ? `%${self.scopeId}` : "");
/**
 * Formats an internet address's host for a native socket API, without brackets
 * or a port.
 *
 * **Details**
 *
 * On `"win32"`, IPv6 zones remain numeric. On other platforms, the first
 * interface name matching the scope ID is used, falling back to the numeric ID
 * when no name matches. Unscoped IPv6 and IPv4 hosts are unchanged.
 *
 * Supply a scope map from {@link scopeIdsFromInterfaces} and optionally a platform
 * string. Omitting the platform uses non-Windows behavior. This function performs
 * no operating-system lookups.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatNativeHost = (self, scopeIds, platform) => {
  if (platform !== "win32" && isInetAddressV6(self) && self.scopeId !== 0) {
    for (const [name, scopeId] of scopeIds) {
      if (scopeId === self.scopeId) return `${formatIp(self.address)}%${name}`;
    }
  }
  return formatHost(self);
};
/**
 * Formats an IPv4 address or IPv6 interface index for a native multicast API.
 *
 * **Details**
 *
 * IPv4 addresses are formatted as numeric IPs. An IPv6 index of zero produces
 * `"::"`. Other indices produce `"::%index"` on `"win32"`, or `"::%name"` on
 * other platforms using the first matching interface name, falling back to the
 * numeric index when no name matches.
 *
 * Supply a scope map from {@link scopeIdsFromInterfaces} and optionally a platform
 * string. Omitting the platform uses non-Windows behavior. This function performs
 * no operating-system lookups.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatMulticastInterface = (networkInterface, scopeIds, platform) => {
  if (typeof networkInterface !== "number") return formatIp(networkInterface);
  if (networkInterface === 0) return "::";
  if (platform !== "win32") {
    for (const [name, scopeId] of scopeIds) {
      if (scopeId === networkInterface) return `::%${name}`;
    }
  }
  return `::%${networkInterface}`;
};
/**
 * Formats a resolved internet address, bracketing IPv6 around its port.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatInet = self => {
  const host = formatHost(self);
  return isInetAddressV4(self) ? `${host}:${self.port}` : `[${host}]:${self.port}`;
};
/**
 * Formats an IP address for use as a URL authority host.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatUrlHost = self => isIpv4Address(self) ? formatIp(self) : `[${formatIp(self)}]`;
/**
 * Converts an IP address or internet socket address to a WHATWG `URL`.
 *
 * **Details**
 *
 * Defaults to the HTTP scheme and uses standard URL normalization, including
 * IPv6 brackets and omission of default ports. Bare IP addresses have no
 * explicit port; internet socket addresses retain their port, including zero.
 * Unspecified addresses are preserved.
 *
 * **Gotchas**
 *
 * Returns a `NetAddressError` for scoped IPv6 addresses or inputs that the URL
 * constructor rejects. Supply the scheme without a trailing colon, for example
 * `"https"`.
 *
 * @stability unstable
 * @category conversions
 * @since 4.0.0
 */
export const toUrl = (self, scheme = "http") => {
  if (self._tag === "InetAddressV6" && self.scopeId !== 0) {
    return addressError(self, "scoped IPv6 addresses are not supported by WHATWG URLs");
  }
  return Result.try({
    try: () => new URL(`${scheme}://${isInetAddress(self) ? formatInet(self) : formatUrlHost(self)}`),
    catch: cause => new NetAddressError({
      input: self,
      message: "failed to construct URL",
      cause
    })
  });
};
/**
 * Formats an IP or socket address as a URL display string.
 *
 * **Details**
 *
 * Defaults to HTTP, brackets IPv6 addresses, and omits default ports and the
 * trailing slash. Custom schemes such as `"tcp"` retain their scheme and host.
 * Unix socket addresses use {@link formatUnixPath}, preserving the raw path
 * with a `unix://` prefix regardless of the supplied scheme.
 *
 * **Gotchas**
 *
 * Returns a `NetAddressError` when URL conversion fails, including for scoped
 * IPv6 addresses.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatUrl = (self, scheme = "http") => isUnixPathAddress(self) ? Result.succeed(formatUnixPath(self)) : Result.map(toUrl(self, scheme), url => `${url.protocol}//${url.host}`);
/**
 * Formats an IP or socket address as a URL display string,
 * throwing a `NetAddressError` when conversion fails.
 *
 * @see {@link formatUrl} for the checked version and formatting behavior.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const formatUrlUnsafe = (self, scheme = "http") => Result.getOrThrow(formatUrl(self, scheme));
/**
 * Formats a hostname or numeric IP address for use as a URL authority host.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatUrlHostString = host => host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
const UnixPathProto = {
  _tag: "UnixPathAddress",
  [TypeId]: TypeId,
  [Equal.symbol](that) {
    return isUnixPathAddress(that) && this.path === that.path;
  },
  [Hash.symbol]() {
    return Hash.combine(Hash.string("UnixPathAddress"), Hash.string(this.path));
  },
  toString() {
    return this.path;
  },
  toJSON() {
    return this.toString();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
/**
 * Creates a Unix-domain filesystem address without normalizing its opaque path.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const unixPathAddress = path => {
  const self = Object.create(UnixPathProto);
  self.path = path;
  return Object.freeze(self);
};
/**
 * Formats a Unix-domain socket path as a readable `unix://path` display string.
 *
 * **Details**
 *
 * Preserves the raw path without URL encoding or normalization. The resulting
 * string is intended for display.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatUnixPath = self => `unix://${self.path}`;
/**
 * Converts a `SocketAddress.Input` to a concrete socket address.
 *
 * **Details**
 *
 * Numeric IP strings are parsed without hostname resolution. Invalid IP
 * literals, ports, and input shapes return a `NetAddressError`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const socketAddressFromInput = input => {
  if (isSocketAddress(input)) return Result.succeed(input);
  if (typeof input === "string") return inetAddressFromString(input);
  if (hasProperty(input, "path")) {
    return typeof input.path === "string" ? Result.succeed(unixPathAddress(input.path)) : addressError(input, "path must be a string");
  }
  if (!hasProperty(input, "address") || !hasProperty(input, "port")) {
    return addressError(input, "expected an address and port or a Unix path");
  }
  const address = typeof input.address === "string" ? ipFromString(input.address) : isIpAddress(input.address) ? Result.succeed(input.address) : addressError(input, "address must be an IP address or numeric IP string");
  return Result.flatMap(address, address => inetAddress(address, input.port));
};
/**
 * Converts a trusted `SocketAddress.Input` to a concrete socket address,
 * throwing on failure.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const socketAddressFromInputUnsafe = input => Result.getOrThrow(socketAddressFromInput(input));
/**
 * Formats a portable socket address for human-readable output.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const formatSocketAddress = self => self._tag === "UnixPathAddress" ? self.path : formatInet(self);
//# sourceMappingURL=NetAddress.js.map