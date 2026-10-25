// src/service-timing.ts
var timings = new WeakMap;
var defaultEnsureTiming2 = {
  pollInterval: 25,
  requestTimeout: 2000,
  spawnDelay: 5000,
  maxSpawnDelay: 30000,
  promiseTimeout: 120000,
  stopPollInterval: 50,
  stopPollAttempts: 100
};
function ensureTiming2(options) {
  return timings.get(options) ?? defaultEnsureTiming2;
}
function withEnsureTiming2(options, overrides) {
  timings.set(options, { ...defaultEnsureTiming2, ...overrides });
  return options;
}

export { defaultEnsureTiming2, ensureTiming2, withEnsureTiming2 };
