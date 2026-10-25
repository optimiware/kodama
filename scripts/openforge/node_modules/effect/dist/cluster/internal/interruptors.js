import * as Effect from "../../Effect.js";
const counts = /*#__PURE__*/new Map();
const acquire = key => {
  counts.set(key, (counts.get(key) ?? 0) + 1);
};
const release = key => {
  const n = counts.get(key);
  if (n === undefined) return;
  if (n <= 1) counts.delete(key);else counts.set(key, n - 1);
};
const around = (key, effect) => Effect.suspend(() => {
  acquire(key);
  return Effect.ensuring(effect, Effect.sync(() => release(key)));
});
const entityKey = address => `entity:${address.entityType}:${address.entityId}:${address.shardId.toString()}`;
const shardKey = shardId => `shard:${shardId.toString()}`;
const entityTypeKey = entityType => `type:${entityType}`;
/**
 * @internal
 */
export const acquireEntity = address => acquire(entityKey(address));
/**
 * @internal
 */
export const releaseEntity = address => release(entityKey(address));
/**
 * @internal
 */
export const acquireEntityType = entityType => acquire(entityTypeKey(entityType));
/**
 * @internal
 */
export const releaseEntityType = entityType => release(entityTypeKey(entityType));
/**
 * @internal
 */
export const aroundEntity = (address, effect) => around(entityKey(address), effect);
/**
 * @internal
 */
export const aroundShard = (shardId, effect) => around(shardKey(shardId), effect);
/**
 * @internal
 */
export const aroundEntityType = (entityType, effect) => around(entityTypeKey(entityType), effect);
/**
 * @internal
 */
export const isActive = address => counts.has(entityKey(address)) || counts.has(shardKey(address.shardId)) || counts.has(entityTypeKey(address.entityType));
/**
 * @internal
 */
export const isShardActive = shardId => counts.has(shardKey(shardId));
//# sourceMappingURL=interruptors.js.map