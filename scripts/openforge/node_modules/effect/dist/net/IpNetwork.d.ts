/**
 * Pure, canonical IPv4 and IPv6 network prefixes using CIDR notation.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Equal from "../Equal.ts";
import * as Hash from "../Hash.ts";
import * as Result from "../Result.ts";
import * as IpInterface from "./IpInterface.ts";
import * as NetAddress from "./NetAddress.ts";
declare const TypeId: "~effect/net/IpNetwork";
/**
 * An immutable canonical IP network prefix. Constructors that retain the exact
 * input address also retain its verified refinements. Operations that derive a
 * different address return only its IPv4 or IPv6 family; revalidate the result
 * before treating it as refined.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface IpNetwork<out A extends NetAddress.IpAddress = NetAddress.IpAddress> extends Equal.Equal, Hash.Hash {
    readonly _tag: "IpNetwork";
    readonly address: A;
    readonly prefixLength: number;
    readonly [TypeId]: typeof TypeId;
    toString(): string;
    toJSON(): string;
}
/**
 * An immutable canonical IPv4 network prefix.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Ipv4Network = IpNetwork<NetAddress.Ipv4Address>;
/**
 * An immutable canonical IPv6 network prefix.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Ipv6Network = IpNetwork<NetAddress.Ipv6Address>;
/**
 * Returns `true` when a value is an IPv4 network prefix.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isIpv4Network: (u: unknown) => u is Ipv4Network;
/**
 * Returns `true` when a value is an IPv6 network prefix.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isIpv6Network: (u: unknown) => u is Ipv6Network;
/**
 * Returns `true` when a value is an IPv4 or IPv6 network prefix.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isIpNetwork: (u: unknown) => u is IpNetwork;
type IpAddressFamily<A extends NetAddress.IpAddress> = A extends NetAddress.Ipv4Address ? NetAddress.Ipv4Address : A extends NetAddress.Ipv6Address ? NetAddress.Ipv6Address : never;
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
export declare const make: <A extends NetAddress.IpAddress>(address: A, prefixLength: number) => Result.Result<IpNetwork<A>, NetAddress.NetAddressError>;
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
export declare const fromAddress: <A extends NetAddress.IpAddress>(address: A, prefixLength: number) => Result.Result<IpNetwork<IpAddressFamily<A>>, NetAddress.NetAddressError>;
/**
 * Returns the canonical network containing an interface address. The derived
 * address retains its IPv4 or IPv6 family but not refinements on the input.
 *
 * @stability unstable
 * @category conversions
 * @since 4.0.0
 */
export declare const fromInterface: <A extends NetAddress.IpAddress>(self: IpInterface.IpInterface<A>) => IpNetwork<IpAddressFamily<A>>;
/**
 * Parses a strict IPv4 network prefix in CIDR notation.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const ipv4FromString: (input: string) => Result.Result<Ipv4Network, NetAddress.NetAddressError>;
/**
 * Parses a strict IPv6 network prefix in CIDR notation.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const ipv6FromString: (input: string) => Result.Result<Ipv6Network, NetAddress.NetAddressError>;
/**
 * Parses a strict IPv4 or IPv6 network prefix in CIDR notation.
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare const fromString: (input: string) => Result.Result<IpNetwork, NetAddress.NetAddressError>;
/**
 * Creates a trusted network prefix, throwing when its address or prefix is invalid.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export declare const makeUnsafe: <A extends NetAddress.IpAddress>(address: A, prefixLength: number) => IpNetwork<A>;
/**
 * Creates the network containing a trusted address, throwing when its prefix is invalid.
 * The derived address retains its IPv4 or IPv6 family but not refinements on
 * the input address.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export declare const fromAddressUnsafe: <A extends NetAddress.IpAddress>(address: A, prefixLength: number) => IpNetwork<IpAddressFamily<A>>;
/**
 * Parses a trusted network prefix in CIDR notation, throwing on failure.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export declare const fromStringUnsafe: (input: string) => IpNetwork;
/**
 * Formats a network prefix using canonical address text and its decimal prefix length.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare const format: (self: IpNetwork) => string;
/**
 * Returns the lowest address in a network prefix.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export declare const firstAddress: <A extends NetAddress.IpAddress>(self: IpNetwork<A>) => A;
/**
 * Returns the numerically greatest address in a network prefix. The derived
 * address retains its IPv4 or IPv6 family but not refinements on the network
 * address.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export declare const lastAddress: <A extends NetAddress.IpAddress>(self: IpNetwork<A>) => IpAddressFamily<A>;
/**
 * Returns the exact number of addresses in a network prefix.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export declare const addressCount: (self: IpNetwork) => bigint;
/**
 * Returns `true` when a network prefix contains an IP address.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export declare const contains: {
    /**
     * Returns `true` when a network prefix contains an IP address.
     *
     * @stability unstable
     * @category predicates
     * @since 4.0.0
     */
    (address: NetAddress.IpAddress): (self: IpNetwork) => boolean;
    /**
     * Returns `true` when a network prefix contains an IP address.
     *
     * @stability unstable
     * @category predicates
     * @since 4.0.0
     */
    (self: IpNetwork, address: NetAddress.IpAddress): boolean;
};
/**
 * Returns `true` when a network prefix contains every address in another network prefix.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export declare const containsNetwork: {
    /**
     * Returns `true` when a network prefix contains every address in another network prefix.
     *
     * @stability unstable
     * @category predicates
     * @since 4.0.0
     */
    (other: IpNetwork): (self: IpNetwork) => boolean;
    /**
     * Returns `true` when a network prefix contains every address in another network prefix.
     *
     * @stability unstable
     * @category predicates
     * @since 4.0.0
     */
    (self: IpNetwork, other: IpNetwork): boolean;
};
/**
 * Returns `true` when two network prefixes contain at least one common address.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export declare const overlaps: {
    /**
     * Returns `true` when two network prefixes contain at least one common address.
     *
     * @stability unstable
     * @category predicates
     * @since 4.0.0
     */
    (other: IpNetwork): (self: IpNetwork) => boolean;
    /**
     * Returns `true` when two network prefixes contain at least one common address.
     *
     * @stability unstable
     * @category predicates
     * @since 4.0.0
     */
    (self: IpNetwork, other: IpNetwork): boolean;
};
export {};
//# sourceMappingURL=IpNetwork.d.ts.map