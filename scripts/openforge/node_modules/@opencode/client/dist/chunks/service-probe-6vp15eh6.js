import {
  isRpcError2,
  isRpcInternalError2
} from "./service-probe-ezt8t3fk.js";

// src/promise/rpc.ts
function makeRpc2(raw, events) {
  return (definition) => {
    const subscribe = (name, options) => {
      if (!Object.hasOwn(definition.events, name))
        throw new Error(`Unknown RPC event: ${definition.id}.${name}`);
      const type = eventType(definition, name);
      return {
        [Symbol.asyncIterator]() {
          const controller = new AbortController;
          const signal = options?.signal ? AbortSignal.any([controller.signal, options.signal]) : controller.signal;
          const iterator = async function* () {
            try {
              for await (const published of events.subscribe({ signal })) {
                if (signal.aborted)
                  return;
                if (published.type !== type)
                  continue;
                yield published;
              }
            } catch (error) {
              if (!signal.aborted)
                throw error;
            } finally {
              controller.abort();
            }
          }();
          return {
            next: () => iterator.next(),
            return: () => {
              controller.abort();
              return iterator.return();
            }
          };
        }
      };
    };
    return Object.assign(Object.fromEntries(Object.keys(definition.methods).map((name) => [
      name,
      async (input, options) => {
        try {
          const result = await raw.rpc.call({
            rpcID: definition.id,
            method: name,
            input,
            location: options?.location
          }, { signal: options?.signal, headers: options?.headers });
          return result.output;
        } catch (error) {
          if (!isRpcError2(error) && !isRpcInternalError2(error))
            throw error;
          throw error.data === undefined ? { type: error.type, message: error.message } : { type: error.type, message: error.message, data: error.data };
        }
      }
    ])), {
      events: {
        subscribe,
        on: (name, handler, options) => {
          const controller = new AbortController;
          const signal = options?.signal ? AbortSignal.any([controller.signal, options.signal]) : controller.signal;
          const source = subscribe(name, { signal });
          (async () => {
            for await (const event of source)
              await handler(event);
          })().catch((error) => console.error(error));
          return () => controller.abort();
        }
      }
    });
  };
}
function eventType(definition, name) {
  return `rpc.${definition.id}.${name}`;
}

export { makeRpc2 };
