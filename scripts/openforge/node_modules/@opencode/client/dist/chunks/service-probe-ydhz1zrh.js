// src/solid/connection.ts
import { batch, onCleanup } from "solid-js";
import { createStore } from "solid-js/store";
var connectTimeout = 2000;
var reconnectDelay = 1000;
var connectionHistoryLimit = 50;
var defaultIdleTimeout2 = 45000;
var foregroundIdleThreshold2 = 20000;
function createClientConnection2(initialApi, options) {
  const abort = new AbortController;
  const history = [];
  const idleTimeout = options.idleTimeout ?? defaultIdleTimeout2;
  const [connection, setConnection] = createStore({ status: "connecting", attempt: 0 });
  let api = initialApi;
  let pending = [];
  let flushTimer;
  let stream;
  let current;
  let run;
  let started = false;
  let generation = 0;
  let lastActivity = 0;
  let forced = false;
  function record(status, attempt, error) {
    history.push({ type: "client.connection", created: Date.now(), data: { status, attempt, error } });
    if (history.length > connectionHistoryLimit)
      history.shift();
  }
  function publish(event) {
    pending.push(event);
    if (flushTimer)
      return;
    flushTimer = setTimeout(() => {
      flushTimer = undefined;
      const events = pending;
      pending = [];
      batch(() => events.forEach(options.onEvent));
    }, options.flushInterval ?? 10);
  }
  async function connect(signal, attempt) {
    let connectedAt;
    const request = new AbortController;
    current = request;
    const cancel = () => request.abort(signal.reason);
    const timeout = setTimeout(() => request.abort(new Error("Timed out connecting to server")), connectTimeout);
    signal.addEventListener("abort", cancel, { once: true });
    let watchdog;
    const touch = () => {
      lastActivity = Date.now();
      if (connectedAt === undefined)
        return;
      clearTimeout(watchdog);
      watchdog = setTimeout(() => request.abort(new Error("Event stream stalled")), idleTimeout);
    };
    try {
      record(attempt === 0 ? "connecting" : "reconnecting", attempt);
      options.log?.info?.("event stream connecting", { attempt });
      const iterator = api.event.subscribe({ signal: request.signal, onActivity: touch })[Symbol.asyncIterator]();
      const first = await iterator.next();
      if (signal.aborted)
        return { error: undefined, connectedAt };
      if (first.done)
        return {
          error: request.signal.reason instanceof Error ? request.signal.reason : new Error("Event stream disconnected"),
          connectedAt
        };
      if (first.value.type !== "server.connected")
        return { error: new Error("Event stream did not start with server.connected"), connectedAt };
      clearTimeout(timeout);
      record("connected", attempt);
      connectedAt = Date.now();
      touch();
      options.log?.info?.("event stream connected");
      publish(first.value);
      setConnection({ status: "connected", attempt: 0, error: undefined });
      while (!signal.aborted) {
        const event = await iterator.next();
        if (signal.aborted)
          return { error: undefined, connectedAt };
        if (event.done)
          return {
            error: request.signal.reason instanceof Error ? request.signal.reason : new Error("Event stream disconnected"),
            connectedAt
          };
        touch();
        if ("durable" in event.value && event.value.durable)
          options.log?.debug?.("event", {
            type: event.value.type,
            aggregateID: event.value.durable.aggregateID,
            seq: event.value.durable.seq
          });
        publish(event.value);
      }
      return { error: undefined, connectedAt };
    } catch (error) {
      return { error, connectedAt };
    } finally {
      request.abort();
      if (current === request)
        current = undefined;
      clearTimeout(timeout);
      clearTimeout(watchdog);
      signal.removeEventListener("abort", cancel);
    }
  }
  async function runStream(active) {
    let attempt = 0;
    while (!abort.signal.aborted && started && generation === active) {
      setConnection({ status: attempt === 0 ? "connecting" : "reconnecting", attempt });
      const controller = new AbortController;
      stream = controller;
      const cancel = () => controller.abort(abort.signal.reason);
      abort.signal.addEventListener("abort", cancel);
      const result = await connect(controller.signal, attempt);
      abort.signal.removeEventListener("abort", cancel);
      if (abort.signal.aborted || !started || generation !== active)
        return;
      if (result.connectedAt !== undefined && Date.now() - result.connectedAt >= reconnectDelay)
        attempt = 0;
      attempt += 1;
      const message = errorMessage(result.error);
      record("disconnected", attempt, message);
      options.log?.info?.("event stream disconnected", { attempt, error: message });
      setConnection({ status: "reconnecting", attempt, error: message });
      if (options.reconnect) {
        const next = await options.reconnect(controller.signal).catch((error) => {
          if (!controller.signal.aborted)
            options.log?.info?.("server resolution failed", { attempt, error: errorMessage(error) });
        });
        if (abort.signal.aborted || controller.signal.aborted || !started || generation !== active)
          return;
        if (next) {
          api = next;
          if (attempt === 1)
            continue;
        }
      }
      if (forced) {
        forced = false;
        continue;
      }
      await wait(reconnectDelay, controller.signal);
    }
  }
  function start() {
    if (started)
      return run;
    started = true;
    forced = false;
    const active = ++generation;
    const previous = run;
    const current = (async () => {
      if (previous)
        await previous;
      await runStream(active);
    })().finally(() => {
      if (run !== current)
        return;
      run = undefined;
    });
    run = current;
    return run;
  }
  function stop() {
    if (!started)
      return;
    started = false;
    generation += 1;
    stream?.abort();
    setConnection({ status: "connecting", attempt: 0, error: undefined });
  }
  function resync(reason) {
    if (!started || connection.status !== "connected")
      return;
    options.log?.info?.("event stream resync", { reason, idle: Date.now() - lastActivity });
    forced = true;
    current?.abort(new Error(reason));
  }
  if (options.pageLifecycle) {
    const pagehide = () => stop();
    const pageshow = () => void start();
    const visibility = () => {
      if (document.visibilityState !== "visible")
        return;
      if (Date.now() - lastActivity < foregroundIdleThreshold2)
        return;
      resync("Page returned to the foreground after the event stream went quiet");
    };
    const online = () => resync("Network connection restored");
    window.addEventListener("pagehide", pagehide);
    window.addEventListener("pageshow", pageshow);
    window.addEventListener("online", online);
    document.addEventListener("visibilitychange", visibility);
    onCleanup(() => {
      window.removeEventListener("pagehide", pagehide);
      window.removeEventListener("pageshow", pageshow);
      window.removeEventListener("online", online);
      document.removeEventListener("visibilitychange", visibility);
    });
  }
  start();
  onCleanup(() => {
    stop();
    abort.abort();
    if (flushTimer)
      clearTimeout(flushTimer);
    pending = [];
  });
  return {
    status: () => connection.status,
    attempt: () => connection.attempt,
    error: () => connection.error,
    internal: {
      history: () => history.slice(),
      resync
    }
  };
}
function errorMessage(error) {
  if (error === undefined)
    return;
  if (error instanceof Error)
    return error.message;
  return String(error);
}
function wait(delay, signal) {
  return new Promise((resolve) => {
    const timer = setTimeout(done, delay);
    signal.addEventListener("abort", done, { once: true });
    function done() {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    }
  });
}

export { defaultIdleTimeout2, foregroundIdleThreshold2, createClientConnection2 };
