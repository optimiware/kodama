/**
 * Pure, canonical IPv4 and IPv6 network prefixes using CIDR notation.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Equal from "../Equal.js";
import { dual } from "../Function.js";
import * as Hash from "../Hash.js";
import { NodeInspectSymbol } from "../Inspectable.js";
import { hasProperty } from "../Predicate.js";
import * as Result from "../Result.js";
import * as IpInterface from "./IpInterface.js";
import * as NetAddress from "./NetAddress.js";
const TypeId = "~effect/net/IpNetwork";
/**
 * Returns `true` when a value is an IPv4 network prefix.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isIpv4Network = u => isIpNetwork(u) && NetAddress.isIpv4Address(u.address);
/**
 * Returns `true` when a value is an IPv6 network prefix.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isIpv6Network = u => isIpNetwork(u) && NetAddress.isIpv6Address(u.address);
/**
 * Returns `true` when a value is an IPv4 or IPv6 network prefix.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isIpNetwork = u => hasProperty(u, TypeId);
const IpNetworkProto = {
  _tag: "IpNetwork",
  [TypeId]: TypeId,
  [Equal.symbol](that) {
    return isIpNetwork(that) && this.prefixLength === that.prefixLength && Equal.equals(this.address, that.address);
  },
  [Hash.symbol]() {
    return Hash.combine(Hash.hash(this.address), Hash.number(this.prefixLength));
  },
  toString() {
    return format(this);
  },
  toJSON() {
    return this.toString();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
const fromInterfaceValue = value => {
  const self = Object.assign(Object.create(IpNetworkProto), {
    address: value.address,
    prefixLength: value.prefixLength
  });
  return Object.freeze(self);
};
const toBytes = address => NetAddress.isIpv4Address(address) ? NetAddress.ipv4ToOctets(address) : NetAddress.ipv6ToOctets(address);
const fromBytes = (address, bytes) => {
  const array = new Uint8Array(bytes);
  return NetAddress.isIpv4Address(address) ? NetAddress.ipv4FromBytesUnsafe(array) : NetAddress.ipv6FromBytesUnsafe(array);
};
const maskBytes = (bytes, prefixLength) => {
  const wholeBytes = Math.floor(prefixLength / 8);
  const partialBits = prefixLength % 8;
  return bytes.map((byte, index) => {
    if (index < wholeBytes) return byte;
    if (index === wholeBytes && partialBits !== 0) return byte & (0xff << 8 - partialBits & 0xff);
    return 0;
  });
};
/**
 * Creates a network prefix when the address has no host bits set.
 *
 * **Gotchas**
 *
 * Use {@link fromAddress} when a host address should be truncated to its containing network.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = (address, prefixLength) => {
  return Result.flatMap(IpInterface.make(address, prefixLength), value => {
    const bytes = toBytes(address);
    const masked = maskBytes(bytes, prefixLength);
    if (bytes.some((byte, index) => byte !== masked[index])) {
      return Result.fail(new NetAddress.NetAddressError({
        input: address,
        message: "address has non-zero host bits"
      }));
    }
    return Result.succeed(fromInterfaceValue(value));
  });
};
/**
 * Creates the canonical network containing an address by clearing its host bits.
 *
 * **Gotchas**
 *
 * The returned network's address can differ from the input address.
 * Its type therefore retains the IPv4 or IPv6 family but not refinements on
 * the input address. Revalidate the derived address to recover a refinement.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const fromAddress = (address, prefixLength) => {
  return Result.map(IpInterface.make(address, prefixLength), value => {
    const masked = fromBytes(address, maskBytes(toBytes(address), prefixLength));
    return fromInterfaceValue(IpInterface.makeUnsafe(masked, value.prefixLength));
  });
};
/**
 * Returns the canonical network containing an interface address. The derived
 * address retains its IPv4 or IPv6 family but not refinements on the input.
 *
 * @stability unstable
 * @category conversions
 * @since 4.0.0
 */
export const fromInterface = self => fromAddressUnsafe(self.address, self.prefixLength);
/**
 * Parses a strict IPv4 network prefix in CIDR notation.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const ipv4FromString = input => {
  return Result.flatMap(IpInterface.ipv4FromString(input, {
    prefix: "required"
  }), value => make(value.address, value.prefixLength));
};
/**
 * Parses a strict IPv6 network prefix in CIDR notation.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const ipv6FromString = input => {
  return Result.flatMap(IpInterface.ipv6FromString(input, {
    prefix: "required"
  }), value => make(value.address, value.prefixLength));
};
/**
 * Parses a strict IPv4 or IPv6 network prefix in CIDR notation.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export const fromString = input => {
  return Result.flatMap(IpInterface.fromString(input, {
    prefix: "required"
  }), value => make(value.address, value.prefixLength));
};
/**
 * Creates a trusted network prefix, throwing when its address or prefix is invalid.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const makeUnsafe = (address, prefixLength) => Result.getOrThrow(make(address, prefixLength));
/**
 * Creates the network containing a trusted address, throwing when its prefix is invalid.
 * The derived address retains its IPv4 or IPv6 family but not refinements on
 * the input address.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const fromAddressUnsafe = (address, prefixLength) => Result.getOrThrow(fromAddress(address, prefixLength));
/**
 * Parses a trusted network prefix in CIDR notation, throwing on failure.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export const fromStringUnsafe = input => Result.getOrThrow(fromString(input));
/**
 * Formats a network prefix using canonical address text and its decimal prefix length.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export const format = self => `${NetAddress.formatIp(self.address)}/${self.prefixLength}`;
/**
 * Returns the lowest address in a network prefix.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const firstAddress = self => self.address;
/**
 * Returns the numerically greatest address in a network prefix. The derived
 * address retains its IPv4 or IPv6 family but not refinements on the network
 * address.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const lastAddress = self => {
  const bytes = toBytes(self.address);
  const networkMask = maskBytes(bytes.map(() => 0xff), self.prefixLength);
  return fromBytes(self.address, bytes.map((byte, index) => byte | networkMask[index] ^ 0xff));
};
/**
 * Returns the exact number of addresses in a network prefix.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const addressCount = self => BigInt(1) << BigInt(NetAddress.width(self.address) - self.prefixLength);
const containsBytes = (self, address) => {
  if (NetAddress.isIpv4Address(self.address) !== NetAddress.isIpv4Address(address)) return false;
  const expected = toBytes(self.address);
  const actual = maskBytes(toBytes(address), self.prefixLength);
  return expected.every((byte, index) => byte === actual[index]);
};
/**
 * Returns `true` when a network prefix contains an IP address.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const contains = /*#__PURE__*/dual(2, containsBytes);
/**
 * Returns `true` when a network prefix contains every address in another network prefix.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const containsNetwork = /*#__PURE__*/dual(2, (self, other) => {
  return self.prefixLength <= other.prefixLength && containsBytes(self, other.address);
});
/**
 * Returns `true` when two network prefixes contain at least one common address.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const overlaps = /*#__PURE__*/dual(2, (self, other) => {
  return self.prefixLength <= other.prefixLength ? containsBytes(self, other.address) : containsBytes(other, self.address);
});
//# sourceMappingURL=IpNetwork.js.map