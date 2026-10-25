import * as Duration from "../../Duration.js";
/**
 * @internal
 */
export const effectiveInterval = config => Duration.min(Duration.fromInputUnsafe(config.shardLockRefreshInterval), Duration.divideUnsafe(Duration.fromInputUnsafe(config.shardLockExpiration), 3));
//# sourceMappingURL=shardLock.js.map