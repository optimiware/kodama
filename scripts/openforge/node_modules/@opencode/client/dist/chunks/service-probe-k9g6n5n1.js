import {
  make7
} from "./service-probe-frbwqjmf.js";
import {
  make6
} from "./service-probe-77ntr0t0.js";
import {
  makeRpc2
} from "./service-probe-6vp15eh6.js";
import {
  __export
} from "./service-probe-9rqn6x4v.js";

// src/promise/client.ts
var exports_client3 = {};
__export(exports_client3, {
  OpenCode: () => exports_client3,
  make: () => make5
});
function make5(options) {
  const raw = make6(options);
  const events = make7((signal, onActivity) => raw.event.subscribe({ signal, onActivity }));
  return {
    ...raw,
    rpc: Object.assign(makeRpc2(raw, events), raw.rpc),
    event: events
  };
}

export { make5, exports_client3 };
