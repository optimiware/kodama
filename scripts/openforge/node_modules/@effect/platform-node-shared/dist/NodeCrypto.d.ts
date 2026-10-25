/**
 * Node-compatible implementation of Effect's `Crypto` service.
 *
 * This module builds the service from `node:crypto`, using `randomBytes` for
 * random data and `createHash` for supported digest algorithms. It exports
 * `make` as the concrete service value and `layer` for providing it through
 * Effect context.
 *
 * @since 1.0.0
 */
import * as EffectCrypto from "effect/Crypto";
import * as Layer from "effect/Layer";
/**
 * The default Node.js Crypto service implementation.
 *
 * @category constructors
 * @since 1.0.0
 */
export declare const make: EffectCrypto.Crypto;
/**
 * Layer that provides the Node.js Crypto service implementation.
 *
 * @category layers
 * @since 1.0.0
 */
export declare const layer: Layer.Layer<EffectCrypto.Crypto>;
//# sourceMappingURL=NodeCrypto.d.ts.map