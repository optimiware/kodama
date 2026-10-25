import * as Channel from "../Channel.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as Exit from "../Exit.js";
import { constVoid, dual, flow } from "../Function.js";
import * as Latch from "../Latch.js";
import * as Layer from "../Layer.js";
import * as Predicate from "../Predicate.js";
import * as Pull from "../Pull.js";
import * as Schema from "../Schema.js";
import * as Scope from "../Scope.js";
import * as Stream from "../Stream.js";
/**
 * Runtime type identifier attached to `Socket` services.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TypeId = "~effect/socket/Socket";
/**
 * Returns `true` when a value is a `Socket`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isSocket = u => Predicate.hasProperty(u, TypeId);
/**
 * Service tag for bidirectional socket transports.
 *
 * **When to use**
 *
 * Use to access or provide the socket implementation used by programs that
 * read and write frames through the Effect environment.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export const Socket = /*#__PURE__*/Context.Service("effect/socket/Socket");
/**
 * Constructs a `Socket` from a reader acquisition and a scoped writer.
 *
 * **Details**
 *
 * The reader must fail a suspended pull when its acquisition scope closes; see
 * `Socket` for why. A reader that leaves a pull blocked forever will hang any
 * consumer that shuts the socket down by closing that scope.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = options => Socket.of({
  [TypeId]: TypeId,
  reader: options.reader,
  writer: options.writer
});
const encoder = /*#__PURE__*/new TextEncoder();
/**
 * Acquires the socket's binary `pull`, encoding any string frames as UTF-8
 * bytes.
 *
 * **Details**
 *
 * When a pulled batch contains no string frames it is returned as-is, so
 * transports that only emit bytes (TCP) pay no per-chunk cost.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const readerBytes = self => Effect.map(self.reader, ({
  pull
}) => Effect.map(pull, chunk => {
  for (let i = 0; i < chunk.length; i++) {
    if (typeof chunk[i] === "string") {
      const out = new Array(chunk.length);
      for (let j = 0; j < chunk.length; j++) {
        const item = chunk[j];
        out[j] = typeof item === "string" ? encoder.encode(item) : item;
      }
      return out;
    }
  }
  return chunk;
}));
/**
 * Acquires the socket's string `pull`, decoding binary frames with the optional
 * text encoding.
 *
 * **Details**
 *
 * The `TextDecoder` is created once per acquisition.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const readerString = (self, encoding) => Effect.map(self.reader, ({
  pull
}) => {
  const decoder = new TextDecoder(encoding);
  return Effect.map(pull, chunk => {
    const out = new Array(chunk.length);
    for (let i = 0; i < chunk.length; i++) {
      const item = chunk[i];
      out[i] = typeof item === "string" ? item : decoder.decode(item);
    }
    return out;
  });
});
const CloseEventTypeId = "~effect/socket/Socket/CloseEvent";
/**
 * Represents a socket close event value carrying a close code and optional
 * reason.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class CloseEvent {
  /**
   * Marks this value as a socket close event for runtime guards.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [CloseEventTypeId];
  code;
  reason;
  constructor(code = 1000, reason) {
    this[CloseEventTypeId] = CloseEventTypeId;
    this.code = code;
    this.reason = reason;
  }
  /**
   * Formats the close code and optional reason for display.
   *
   * @stability unstable
   * @since 4.0.0
   */
  toString() {
    return this.reason ? `${this.code}: ${this.reason}` : `${this.code}`;
  }
}
/**
 * Returns `true` when a value is a `CloseEvent`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isCloseEvent = u => Predicate.hasProperty(u, CloseEventTypeId);
/**
 * Runtime type identifier attached to `SocketError` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const SocketErrorTypeId = "~effect/socket/Socket/SocketError";
/**
 * Returns `true` when a value is a `SocketError`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isSocketError = u => Predicate.hasProperty(u, SocketErrorTypeId);
/**
 * Typed error for failures that occur while reading from a socket.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class SocketReadError extends /*#__PURE__*/Schema.Error("effect/socket/Socket/SocketReadError")({
  _tag: /*#__PURE__*/Schema.tag("SocketReadError"),
  cause: /*#__PURE__*/Schema.Defect()
}) {
  /**
   * Default message used for socket read failures.
   *
   * @stability unstable
   * @since 4.0.0
   */
  message = `An error occurred during Read`;
}
/**
 * Typed error for failures that occur while writing to a socket.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class SocketWriteError extends /*#__PURE__*/Schema.Error("effect/socket/Socket/SocketWriteError")({
  _tag: /*#__PURE__*/Schema.tag("SocketWriteError"),
  cause: /*#__PURE__*/Schema.Defect()
}) {
  /**
   * Default message used for socket write failures.
   *
   * @stability unstable
   * @since 4.0.0
   */
  message = `An error occurred during Write`;
}
/**
 * Typed error for failures that occur while opening a socket, including
 * unknown open failures and open timeouts.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class SocketOpenError extends /*#__PURE__*/Schema.Error("effect/socket/Socket/SocketOpenError")({
  _tag: /*#__PURE__*/Schema.tag("SocketOpenError"),
  kind: /*#__PURE__*/Schema.Literals(["Unknown", "Timeout"]),
  cause: /*#__PURE__*/Schema.Defect()
}) {
  /**
   * Formats timeout and unknown open failures for display.
   *
   * @stability unstable
   * @since 4.0.0
   */
  get message() {
    return this.kind === "Timeout" ? `timeout waiting for "open"` : `An error occurred during Open`;
  }
}
/**
 * Typed error for an unsupported or failed in-place TLS upgrade.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class SocketUpgradeError extends /*#__PURE__*/Schema.Error("effect/socket/Socket/SocketUpgradeError")({
  _tag: /*#__PURE__*/Schema.tag("SocketUpgradeError"),
  cause: /*#__PURE__*/Schema.optional(/*#__PURE__*/Schema.Defect())
}) {
  /**
   * An upgrade implementation for transports that cannot wrap the connection
   * with TLS.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static unsupported = () => Effect.fail(new SocketError({
    reason: new SocketUpgradeError({})
  }));
  get message() {
    return this.cause === undefined ? `Socket does not support TLS upgrade` : `An error occurred during TLS upgrade`;
  }
}
/**
 * Typed error for a socket close, carrying the close code and optional close
 * reason.
 *
 * **Details**
 *
 * Sockets never classify closes: any close, whatever the code, fails the
 * reader with a `SocketError` wrapping this reason. Consumers that treat a
 * close as normal catch it.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class SocketCloseError extends /*#__PURE__*/Schema.Error("effect/socket/Socket/SocketCloseError")({
  _tag: /*#__PURE__*/Schema.tag("SocketCloseError"),
  code: Schema.Int,
  closeReason: /*#__PURE__*/Schema.optional(Schema.String)
}) {
  get message() {
    if (this.closeReason) {
      return `${this.code}: ${this.closeReason}`;
    }
    return `${this.code}`;
  }
}
/**
 * Schema for all socket-specific error reasons.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export const SocketErrorReason = /*#__PURE__*/Schema.Union([SocketReadError, SocketWriteError, SocketOpenError, SocketUpgradeError, SocketCloseError]);
/**
 * Tagged error that wraps socket read, write, open, upgrade, and close failures
 * while preserving the underlying reason.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class SocketError extends /*#__PURE__*/Schema.TaggedError(SocketErrorTypeId)("SocketError", {
  _tag: /*#__PURE__*/Schema.tag("SocketError"),
  reason: SocketErrorReason
}) {
  // @effect-diagnostics-next-line overriddenSchemaConstructor:off
  constructor(props) {
    if ("cause" in props.reason) {
      super({
        ...props,
        cause: props.reason.cause
      });
    } else {
      super(props);
    }
  }
  /**
   * Marks this value as a socket error wrapper for runtime guards.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [SocketErrorTypeId] = SocketErrorTypeId;
  /**
   * Returns `true` when the value is a `SocketError`.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static is(u) {
    return isSocketError(u);
  }
  message = this.reason.message;
}
const closeError = (code, closeReason) => new SocketError({
  reason: new SocketCloseError({
    code,
    closeReason
  })
});
const writeChunk = (writer, chunk) => {
  for (let i = 0; i < chunk.length; i++) {
    if (isCloseEvent(chunk[i])) {
      return Effect.gen(function* () {
        for (let index = 0; index <= i; index++) {
          yield* writer.write(chunk[index]);
        }
      });
    }
  }
  return writer.writeAll(chunk);
};
const toChannelWithReader = (self, reader) => Channel.fromTransform(Effect.fnUntraced(function* (upstream, scope) {
  const readScope = yield* Scope.fork(scope);
  const pull = yield* Scope.provide(reader, readScope);
  const writeScope = yield* Scope.fork(scope);
  const writer = yield* Scope.provide(self.writer, writeScope);
  let writeFailure;
  yield* upstream.pipe(Effect.flatMap(chunk => writeChunk(writer, chunk)), Effect.forever({
    disableYield: true
  }), Effect.catchCauseFilter(Pull.filterNoDone, cause => Effect.suspend(() => {
    writeFailure = cause;
    // closing the read scope fails a suspended pull, which is the
    // reader contract, so the read side needs no per-pull failure race
    return Scope.close(readScope, Exit.void);
  })), Effect.ensuring(Scope.close(writeScope, Exit.void)), Effect.forkIn(scope));
  // @effect-diagnostics-next-line returnEffectInGen:off
  return Effect.catchCause(Effect.suspend(() => writeFailure !== undefined ? Effect.failCause(writeFailure) : pull), cause => Effect.failCause(writeFailure ?? cause));
}));
/**
 * Converts a `Socket` into a bidirectional binary `Channel`, encoding
 * incoming string frames as UTF-8 bytes and writing outgoing frame batches to
 * the socket.
 *
 * **Details**
 *
 * The read side is the socket's pull, so the channel is backpressured
 * end-to-end.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const toChannel = self => toChannelWithReader(self, readerBytes(self));
/**
 * Converts a `Socket` into a bidirectional string `Channel`, decoding binary
 * frames with the optional text encoding.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const toChannelString = /*#__PURE__*/dual(args => isSocket(args[0]), (self, encoding) => toChannelWithReader(self, readerString(self, encoding)));
/**
 * Creates a `Socket` to binary `Channel` adapter with a fixed upstream error
 * type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const toChannelWith = () => self => toChannel(self);
/**
 * Converts a `Socket` into a read-only binary `Stream` backed by the socket's
 * pull, so consumption is backpressured end-to-end.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const toStream = self => Stream.fromChannel(Channel.fromTransform((_, scope) => Scope.provide(readerBytes(self), scope)));
/**
 * Creates a binary socket `Channel` from the `Socket` service in the
 * environment.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeChannel = () => Channel.unwrap(Effect.map(Socket, toChannelWith()));
/**
 * Context service for the active `WebSocket` instance.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class WebSocket extends /*#__PURE__*/Context.Service()("~effect/socket/Socket/WebSocket") {}
/**
 * Context service for constructing `WebSocket` instances from a URL and
 * optional protocols or platform-specific options.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class WebSocketConstructor extends /*#__PURE__*/Context.Service()("@effect/platform/Socket/WebSocketConstructor") {}
/**
 * Layer that provides `WebSocketConstructor` using `globalThis.WebSocket`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerWebSocketConstructorGlobal = /*#__PURE__*/Layer.succeed(WebSocketConstructor)((url, options) => {
  if (options !== undefined && typeof options !== "string" && !Array.isArray(options)) {
    throw new TypeError("WebSocket client options are not supported by the global WebSocket constructor");
  }
  return new globalThis.WebSocket(url, options);
});
/**
 * Creates a `Socket` backed by a `WebSocketConstructor`, dialing the
 * WebSocket for each reader acquisition.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeWebSocket = (url, options) => WebSocketConstructor.use(makeWs => fromWebSocket(Effect.acquireRelease((typeof url === "string" ? Effect.succeed(url) : url).pipe(Effect.map(url => makeWs(url, options?.protocols))), ws => Effect.sync(() => ws.close(1000))), options));
const isPausable = ws => "pause" in ws && typeof ws.pause === "function";
/**
 * Builds a `Socket` from a scoped WebSocket acquisition effect.
 *
 * **Details**
 *
 * Reader acquisition runs `acquire`, attaches event listeners, then waits for
 * the socket to open. Implementations exposing `pause`/`resume` (the `ws`
 * package) remain flowing until buffered frames reach the configured
 * `highWaterMark` (64 KiB by default). Draining the buffer resumes the
 * transport. Incoming frames that arrive in the same tick are coalesced into
 * one batch. Implementations without pause (browsers) buffer incoming frames,
 * optionally failing the socket with a `SocketReadError` when `highWaterMark`
 * bytes are exceeded (default unbounded). Message boundaries survive: each
 * pulled batch contains one element per frame.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const fromWebSocket = (acquire, options) => Effect.withFiber(fiber => {
  let currentWS;
  const latch = Latch.makeUnsafe(false);
  const acquireContext = fiber.context;
  const reader = Effect.gen(function* () {
    const scope = yield* Effect.scope;
    const ws = yield* Scope.provide(acquire, scope);
    if ("binaryType" in ws) {
      ;
      ws.binaryType = "arraybuffer";
    }
    const pausable = isPausable(ws);
    const highWaterMark = options?.highWaterMark ?? (pausable ? defaultHighWaterMark : undefined);
    let open = ws.readyState === 1;
    let paused = pausable && "isPaused" in ws && ws.isPaused === true;
    let buffer = [];
    let bufferSize = 0;
    let error;
    let waiter;
    let openWaiter;
    function pauseWebSocket() {
      if (!pausable || paused) return;
      ws.pause();
      paused = true;
    }
    function resumeWebSocket() {
      if (!pausable || !paused) return;
      ws.resume();
      paused = false;
    }
    function takeBuffer() {
      const chunk = buffer;
      buffer = [];
      bufferSize = 0;
      if (error === undefined) resumeWebSocket();
      return chunk;
    }
    function deliver() {
      if (waiter === undefined || buffer.length === 0) return;
      const resume = waiter;
      waiter = undefined;
      resume(Effect.succeed(takeBuffer()));
    }
    function push(data) {
      buffer.push(data);
      if (highWaterMark !== undefined) {
        bufferSize += typeof data === "string" ? encoder.encode(data).byteLength : data.byteLength;
      }
      if (waiter !== undefined) deliver();
      if (pausable && bufferSize >= highWaterMark) {
        pauseWebSocket();
      } else if (!pausable && !waiter && highWaterMark !== undefined && bufferSize > highWaterMark) {
        fail(new SocketError({
          reason: new SocketReadError({
            cause: new Error(`Socket highWaterMark of ${highWaterMark} bytes exceeded`)
          })
        }));
      }
    }
    function fail(err) {
      if (error === undefined) error = err;
      if (openWaiter !== undefined) {
        const {
          cleanup,
          resume
        } = openWaiter;
        openWaiter = undefined;
        cleanup();
        resume(Effect.fail(error));
      }
      if (waiter !== undefined) {
        const resume = waiter;
        waiter = undefined;
        resume(buffer.length > 0 ? Effect.succeed(takeBuffer()) : Effect.fail(error));
      }
    }
    function onMessage(event) {
      const data = event.data;
      if (typeof Blob !== "undefined" && data instanceof Blob) {
        data.arrayBuffer().then(buf => push(new Uint8Array(buf)), cause => {
          fail(new SocketError({
            reason: new SocketReadError({
              cause
            })
          }));
        });
        return;
      }
      push(data instanceof ArrayBuffer ? new Uint8Array(data) : data);
    }
    function onError(event) {
      fail(new SocketError({
        reason: open ? new SocketReadError({
          cause: event
        }) : new SocketOpenError({
          kind: "Unknown",
          cause: event
        })
      }));
    }
    function onClose(event) {
      fail(closeError(typeof event.code === "number" ? event.code : 1006, event.reason));
    }
    ws.addEventListener("message", onMessage);
    ws.addEventListener("error", onError, {
      once: true
    });
    ws.addEventListener("close", onClose, {
      once: true
    });
    yield* Scope.addFinalizer(scope, Effect.sync(() => {
      // resume a pull blocked in another fiber before detaching
      fail(closeError(1006));
      ws.removeEventListener("message", onMessage);
      ws.removeEventListener("error", onError);
      ws.removeEventListener("close", onClose);
      // let the close handshake proceed once nothing is pulling
      if (pausable) {
        try {
          resumeWebSocket();
        } catch {
          // the underlying stream may already be gone
        }
      }
      latch.closeUnsafe();
      currentWS = undefined;
    }));
    if (ws.readyState >= 2) {
      // @effect-diagnostics-next-line unnecessaryFailYieldableError:off
      return yield* Effect.fail(error ?? closeError(1006));
    }
    if (!open) {
      yield* Effect.callback(resume => {
        const onOpen = () => {
          if (openWaiter?.resume !== resume) return;
          openWaiter = undefined;
          ws.removeEventListener("open", onOpen);
          resume(Effect.void);
        };
        const cleanup = () => ws.removeEventListener("open", onOpen);
        openWaiter = {
          resume,
          cleanup
        };
        ws.addEventListener("open", onOpen, {
          once: true
        });
        return Effect.sync(() => {
          if (openWaiter?.resume === resume) openWaiter = undefined;
          cleanup();
        });
      }).pipe(Effect.timeoutOrElse({
        duration: options?.openTimeout ?? 10000,
        orElse: () => Effect.fail(new SocketError({
          reason: new SocketOpenError({
            kind: "Timeout",
            cause: new Error("timeout waiting for \"open\"")
          })
        }))
      }));
      open = true;
    }
    if (error === undefined && bufferSize < highWaterMark) resumeWebSocket();
    currentWS = ws;
    latch.openUnsafe();
    return {
      pull: Effect.callback(resume => {
        if (buffer.length > 0) return resume(Effect.succeed(takeBuffer()));
        if (error !== undefined) return resume(Effect.fail(error));
        waiter = resume;
        return Effect.sync(() => {
          if (waiter === resume) waiter = undefined;
        });
      }),
      upgrade: SocketUpgradeError.unsupported
    };
  }).pipe(Effect.updateContext(input => Context.merge(acquireContext, input)));
  const write = chunk => Effect.suspend(() => {
    const ws = currentWS;
    if (ws === undefined) return latch.whenOpen(write(chunk));
    try {
      if (isCloseEvent(chunk)) {
        ws.close(chunk.code, chunk.reason);
      } else {
        ws.send(chunk);
      }
      return Effect.void;
    } catch (cause) {
      return Effect.fail(new SocketError({
        reason: new SocketWriteError({
          cause
        })
      }));
    }
  });
  const writeAll = chunks => Effect.suspend(() => {
    const ws = currentWS;
    if (ws === undefined) return latch.whenOpen(writeAll(chunks));
    try {
      for (let i = 0; i < chunks.length; i++) {
        ws.send(chunks[i]);
      }
      return Effect.void;
    } catch (cause) {
      return Effect.fail(new SocketError({
        reason: new SocketWriteError({
          cause
        })
      }));
    }
  });
  const writer = Effect.succeed({
    write,
    writeAll
  });
  return Effect.succeed(make({
    reader,
    writer
  }));
});
const defaultHighWaterMark = 64 * 1024;
/**
 * Creates a binary `Channel` backed by a WebSocket URL, requiring a
 * `WebSocketConstructor` service.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeWebSocketChannel = (url, options) => Channel.unwrap(Effect.map(makeWebSocket(url, options), toChannelWith()));
/**
 * Layer that provides a `Socket` service backed by a WebSocket URL or URL
 * effect.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerWebSocket = /*#__PURE__*/flow(makeWebSocket, /*#__PURE__*/Layer.effect(Socket));
/**
 * Builds a `Socket` from a scoped `InputTransformStream`, pulling incoming
 * chunks from the readable side and writing outgoing chunks to the writable
 * stream, encoding strings as UTF-8.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const fromTransformStream = acquire => Effect.withFiber(fiber => {
  const latch = Latch.makeUnsafe(false);
  let currentStream;
  const acquireContext = fiber.context;
  const writers = new WeakMap();
  const getWriter = stream => {
    let writer = writers.get(stream);
    if (!writer) {
      writer = stream.writable.getWriter();
      writers.set(stream, writer);
    }
    return writer;
  };
  const reader = Effect.gen(function* () {
    const scope = yield* Effect.scope;
    const stream = yield* Scope.provide(acquire, scope);
    const readerHandle = stream.readable.getReader();
    let error;
    yield* Scope.addFinalizer(scope, Effect.suspend(() => {
      latch.closeUnsafe();
      currentStream = undefined;
      return Effect.promise(() => readerHandle.cancel().catch(constVoid));
    }));
    currentStream = {
      stream,
      fail(err) {
        if (error === undefined) error = err;
        readerHandle.cancel().catch(constVoid);
      }
    };
    latch.openUnsafe();
    const read = Effect.tryPromise({
      try: () => readerHandle.read(),
      catch: cause => error ?? new SocketError({
        reason: new SocketReadError({
          cause
        })
      })
    });
    return {
      pull: Effect.suspend(() => {
        if (error !== undefined) return Effect.fail(error);
        return Effect.flatMap(read, ({
          done,
          value
        }) => done ? Effect.fail(error ?? closeError(1000)) : Effect.succeed([value]));
      }),
      upgrade: SocketUpgradeError.unsupported
    };
  }).pipe(Effect.updateContext(input => Context.merge(acquireContext, input)));
  const write = chunk => latch.whenOpen(Effect.suspend(() => {
    const current = currentStream;
    if (isCloseEvent(chunk)) {
      return Effect.sync(() => {
        current.fail(closeError(chunk.code, chunk.reason));
      });
    }
    return Effect.tryPromise({
      try: () => getWriter(current.stream).write(typeof chunk === "string" ? encoder.encode(chunk) : chunk),
      catch: cause => new SocketError({
        reason: new SocketWriteError({
          cause
        })
      })
    });
  }));
  const writeAll = chunks => latch.whenOpen(Effect.tryPromise({
    try: async () => {
      const writer = getWriter(currentStream.stream);
      for (let i = 0; i < chunks.length; i++) {
        const chunk = chunks[i];
        await writer.write(typeof chunk === "string" ? encoder.encode(chunk) : chunk);
      }
    },
    catch: cause => new SocketError({
      reason: new SocketWriteError({
        cause
      })
    })
  }));
  const writer = Effect.acquireRelease(Effect.succeed({
    write,
    writeAll
  }), () => Effect.promise(async () => {
    if (!currentStream) return;
    await getWriter(currentStream.stream).close().catch(constVoid);
  }));
  return Effect.succeed(make({
    reader,
    writer
  }));
});
//# sourceMappingURL=Socket.js.map